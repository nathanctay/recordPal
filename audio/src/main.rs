use hound::{SampleFormat, WavReader};
use json::object;
use std::io::{Cursor, Write};
use std::os::unix::net::UnixStream;

const SOCKET_PATH: &str = "/tmp/nowplaying.sock";
const DEFAULT_CLIP: &str = "sample.wav";

fn main() {
    // Drop a 10–15s WAV here (or pass a path) to test against AudD.
    // Real worker: cpal captures PCM → wrap as WAV → broadcast_clip.
    let path = std::env::args()
        .nth(1)
        .unwrap_or_else(|| DEFAULT_CLIP.to_string());

    let (wav, duration_s, rms_energy) = match load_clip(&path) {
        Ok(v) => v,
        Err(e) => {
            eprintln!("failed to load {path}: {e}");
            eprintln!("place a WAV at {DEFAULT_CLIP} or run: cargo run -- /path/to/clip.wav");
            std::process::exit(1);
        }
    };

    match broadcast_clip(&wav, duration_s, rms_energy) {
        Ok(()) => println!(
            "sent {duration_s}s clip ({} bytes) to {SOCKET_PATH}",
            wav.len()
        ),
        Err(e) => {
            eprintln!("broadcast failed: {e}");
            std::process::exit(1);
        }
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
        SampleFormat::Int => {
            for sample in reader.samples::<i32>() {
                let n = sample.map_err(std::io::Error::other)? as f64 / scale;
                sum_sq += n * n;
                count += 1;
            }
        }
        SampleFormat::Float => {
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
