import { useEffect, useState } from 'react'
import './App.css'

type Track = {
  title: string
  artist: string
  duration_s: number
  album: string
  album_cover_url: string
  release_date?: string
}

const State = {
  Idle: 'idle',
  Listening: 'listening',
  Identifying: 'identifying',
  Error: 'error',
} as const
export type State = (typeof State)[keyof typeof State]

type StatusMeta = {
  label: string
  tone: 'idle' | 'active' | 'error'
  animated: boolean
}

const STATUS: Record<State, StatusMeta> = {
  idle: { label: 'Idle', tone: 'idle', animated: false },
  listening: { label: 'Listening', tone: 'active', animated: true },
  identifying: { label: 'Identifying', tone: 'active', animated: true },
  error: { label: 'Error', tone: 'error', animated: false },
}

function formatDuration(totalSeconds: number): string {
  if (!totalSeconds || totalSeconds < 0) return '0:00'
  const minutes = Math.floor(totalSeconds / 60)
  const seconds = totalSeconds % 60
  return `${minutes}:${seconds < 10 ? '0' : ''}${seconds}`
}

function yearOf(releaseDate?: string): string | undefined {
  if (!releaseDate) return undefined
  const match = releaseDate.match(/\d{4}/)
  return match?.[0]
}

function App() {
  const [currentTrack, setCurrentTrack] = useState<Track>()
  const [currentState, setCurrentState] = useState<State>('idle')

  useEffect(() => {
    const trackSource = new EventSource('http://localhost:8080/tracks')
    const stateSource = new EventSource('http://localhost:8080/state')

    trackSource.addEventListener('track', (e) => {
      setCurrentTrack(JSON.parse(e.data))
    })
    stateSource.addEventListener('state', (e) => {
      setCurrentState(JSON.parse(e.data))
    })

    return () => {
      trackSource.close()
      stateSource.close()
    }
  }, [])

  // The middleware can emit an empty track on a no-match; treat anything
  // without a title as "nothing playing" and show the resting state.
  const hasTrack = Boolean(currentTrack?.title)
  const status = STATUS[currentState] ?? STATUS.idle
  const isSpinning = hasTrack && currentState !== 'idle'
  const year = yearOf(currentTrack?.release_date)

  return (
    <div className="stage" data-spin={isSpinning}>
      {hasTrack && currentTrack?.album_cover_url ? (
        <div
          key={currentTrack.title}
          className="ambient"
          style={{ backgroundImage: `url(${currentTrack.album_cover_url})` }}
        />
      ) : (
        <div className="ambient is-empty" />
      )}
      <div className="veil" />

      <main className="player">
        <section className="turntable artwork-in" key={`art-${currentTrack?.title ?? 'idle'}`}>
          <div className="vinyl" />
          <div className="sleeve">
            {hasTrack && currentTrack?.album_cover_url ? (
              <img
                src={currentTrack.album_cover_url}
                alt={`${currentTrack.album} cover`}
              />
            ) : (
              <div className="sleeve-empty">
                <svg viewBox="0 0 24 24" fill="none" aria-hidden="true">
                  <path
                    d="M9 18V6l10-2v12"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  />
                  <circle cx="6" cy="18" r="3" stroke="currentColor" strokeWidth="1.5" />
                  <circle cx="16" cy="16" r="3" stroke="currentColor" strokeWidth="1.5" />
                </svg>
              </div>
            )}
          </div>
        </section>

        <section className="meta animate-in" key={currentTrack?.title ?? 'idle'}>
          <div className="status" data-tone={status.tone} data-animated={status.animated}>
            {status.animated ? (
              <span className="eq" aria-hidden="true">
                <span />
                <span />
                <span />
                <span />
              </span>
            ) : (
              <span className="steady-dot" aria-hidden="true" />
            )}
            {status.label}
          </div>

          {hasTrack ? (
            <>
              <p className="eyebrow">Now Playing</p>
              <h1 className="title">{currentTrack!.title}</h1>
              <p className="artist">{currentTrack!.artist}</p>
              <div className="subline">
                {currentTrack!.album && <span className="album-name">{currentTrack!.album}</span>}
                {year && (
                  <>
                    <span className="dot" />
                    <span>{year}</span>
                  </>
                )}
              </div>
              <div className="rail">
                <div className="rail-track">
                  <div className="rail-fill" />
                </div>
                <div className="rail-time">
                  <span>--:--</span>
                  <span>{formatDuration(currentTrack!.duration_s)}</span>
                </div>
              </div>
            </>
          ) : (
            <>
              <p className="eyebrow">RecordPal</p>
              <h1 className="title">Waiting for the drop</h1>
              <p className="artist">Play a record and I&rsquo;ll name the tune.</p>
            </>
          )}
        </section>
      </main>
    </div>
  )
}

export default App
