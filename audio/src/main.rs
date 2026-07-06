use cpal::traits::{DeviceTrait, HostTrait, StreamTrait};
use cpal::{Error, FromSample, Sample, SampleFormat, SupportedStreamConfig};
use hound::{WavReader, WavWriter};
use json::object;
use std::fs::File;
use std::io::{BufWriter, Cursor, Write};
use std::os::unix::net::UnixStream;
use std::sync::{Arc, Mutex};

const SOCKET_PATH: &str = "/tmp/nowplaying.sock";
const CLIP_PATH: &str = "clip.wav";
const RECORD_SECS: u64 = 12;
// Normalized RMS below this counts as silence — we skip the AudD call entirely.
const SILENCE_THRESHOLD: f32 = 0.003;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Testing: `cargo run -- some.wav` sends one existing clip and exits.
    if let Some(path) = std::env::args().nth(1) {
        let (wav, duration_s, rms_energy) = load_clip(&path)?;
        broadcast_clip(&wav, duration_s, rms_energy)?;
        println!("sent {duration_s}s clip ({} bytes)", wav.len());
        return Ok(());
    }

    // Worker: record → gate on loudness → send → repeat, forever.
    run_capture_loop()
}

/// Continuously capture clips and forward loud ones to the middleware.
fn run_capture_loop() -> Result<(), Box<dyn std::error::Error>> {
    loop {
        if let Err(e) = record_to_file(CLIP_PATH, RECORD_SECS) {
            eprintln!("record error: {e}");
            std::thread::sleep(std::time::Duration::from_secs(1));
            continue;
        }

        let (wav, duration_s, rms_energy) = match load_clip(CLIP_PATH) {
            Ok(clip) => clip,
            Err(e) => {
                eprintln!("read error: {e}");
                continue;
            }
        };

        // Silence gate: don't spend an AudD lookup on a quiet room.
        if rms_energy < SILENCE_THRESHOLD {
            println!("silence (rms {rms_energy:.4}) — skipping");
            continue;
        }

        match broadcast_clip(&wav, duration_s, rms_energy) {
            Ok(()) => println!(
                "sent {duration_s}s clip (rms {rms_energy:.4}, {} bytes)",
                wav.len()
            ),
            Err(e) => eprintln!("broadcast error: {e}"),
        }
    }
}

/// Record `seconds` of audio from the default input device into a WAV file.
fn record_to_file(path: &str, seconds: u64) -> Result<(), Box<dyn std::error::Error>> {
    let host = cpal::default_host();
    let device = host
        .default_input_device()
        .ok_or("no default input device")?;
    let config = device.default_input_config()?;
    println!("recording {seconds}s from default mic ({config:?})");

    let spec = wav_spec_from_config(&config);
    let writer = WavWriter::create(path, spec)?;
    let writer = Arc::new(Mutex::new(Some(writer)));
    let writer_for_cb = writer.clone();

    let err_fn = move |err: Error| eprintln!("stream error: {err}");

    // The mic decides the sample format at runtime, so build the stream with a
    // callback typed for whichever format it reports.
    let stream = match config.sample_format() {
        SampleFormat::I8 => device.build_input_stream(
            config.into(),
            move |data, _: &_| write_input_data::<i8, i8>(data, &writer_for_cb),
            err_fn,
            None,
        )?,
        SampleFormat::I16 => device.build_input_stream(
            config.into(),
            move |data, _: &_| write_input_data::<i16, i16>(data, &writer_for_cb),
            err_fn,
            None,
        )?,
        SampleFormat::I32 => device.build_input_stream(
            config.into(),
            move |data, _: &_| write_input_data::<i32, i32>(data, &writer_for_cb),
            err_fn,
            None,
        )?,
        SampleFormat::F32 => device.build_input_stream(
            config.into(),
            move |data, _: &_| write_input_data::<f32, f32>(data, &writer_for_cb),
            err_fn,
            None,
        )?,
        other => return Err(format!("unsupported sample format {other:?}").into()),
    };

    stream.play()?;
    std::thread::sleep(std::time::Duration::from_secs(seconds));
    drop(stream); // stops capture; no more callbacks fire

    // Take the writer out of the shared cell so we can consume it in finalize().
    writer
        .lock()
        .unwrap()
        .take()
        .expect("writer already taken")
        .finalize()?;
    Ok(())
}

/// Copy captured samples into the WAV writer. Runs on the audio thread
fn write_input_data<T, U>(input: &[T], writer: &WavWriterHandle)
where
    T: Sample,
    U: Sample + hound::Sample + FromSample<T>,
{
    if let Ok(mut guard) = writer.try_lock() {
        if let Some(writer) = guard.as_mut() {
            for &sample in input.iter() {
                let sample: U = U::from_sample(sample);
                writer.write_sample(sample).ok();
            }
        }
    }
}

type WavWriterHandle = Arc<Mutex<Option<WavWriter<BufWriter<File>>>>>;

fn wav_spec_from_config(config: &SupportedStreamConfig) -> hound::WavSpec {
    hound::WavSpec {
        channels: config.channels() as _,
        sample_rate: config.sample_rate() as _,
        bits_per_sample: (config.sample_format().sample_size() * 8) as _,
        sample_format: to_hound_format(config.sample_format()),
    }
}

fn to_hound_format(format: SampleFormat) -> hound::SampleFormat {
    if format.is_float() {
        hound::SampleFormat::Float
    } else {
        hound::SampleFormat::Int
    }
}

fn load_clip(path: &str) -> std::io::Result<(Vec<u8>, u32, f32)> {
    let wav = std::fs::read(path)?;
    let duration_s = wav_duration_seconds(&wav)?;
    let rms_energy = wav_rms_energy(&wav)?;
    Ok((wav, duration_s, rms_energy))
}

fn wav_duration_seconds(wav: &[u8]) -> std::io::Result<u32> {
    let reader = WavReader::new(Cursor::new(wav)).map_err(std::io::Error::other)?;
    let secs = reader.duration() as f32 / reader.spec().sample_rate as f32;
    Ok(secs.round().max(1.0) as u32)
}

fn wav_rms_energy(wav: &[u8]) -> std::io::Result<f32> {
    let mut reader = WavReader::new(Cursor::new(wav)).map_err(std::io::Error::other)?;
    let spec = reader.spec();
    let scale = ((1_i64 << (spec.bits_per_sample - 1)) - 1) as f64;

    let mut sum_sq = 0.0;
    let mut count = 0u64;

    match spec.sample_format {
        hound::SampleFormat::Int => {
            for sample in reader.samples::<i32>() {
                let n = sample.map_err(std::io::Error::other)? as f64 / scale;
                sum_sq += n * n;
                count += 1;
            }
        }
        hound::SampleFormat::Float => {
            for sample in reader.samples::<f32>() {
                let n = sample.map_err(std::io::Error::other)? as f64;
                sum_sq += n * n;
                count += 1;
            }
        }
    }

    Ok(if count == 0 {
        0.0
    } else {
        (sum_sq / count as f64).sqrt() as f32
    })
}

/// Framed IPC: newline-terminated JSON header, then raw WAV bytes.
fn broadcast_clip(wav: &[u8], duration_s: u32, rms_energy: f32) -> std::io::Result<()> {
    let header = object! {
        "type" => "clip",
        "format" => "wav",
        "duration_s" => duration_s,
        "rms_energy" => rms_energy,
        "bytes" => wav.len(),
    };

    let mut stream = UnixStream::connect(SOCKET_PATH)?;
    stream.write_all(header.dump().as_bytes())?;
    stream.write_all(b"\n")?;
    stream.write_all(wav)?;
    stream.flush()?;
    Ok(())
}
