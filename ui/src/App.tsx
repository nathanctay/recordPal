import { useEffect, useState } from 'react'
import './App.css'

type Track = {
  title: string
  artist: string
  duration_s: number
  album: string
  album_cover_url: string
}
const State = {
  Idle: "idle",
  Listening: "listening",
  Identifying: "identifying",
  Error: "error"
} as const
export type State = typeof State[keyof typeof State];


function App() {
  const [currentTrack, setCurrentTrack] = useState<Track>()
  const [currentState, setCurrentState] = useState<State>()
  useEffect(() => {
    const trackSource = new EventSource('http://localhost:8080/tracks')
    const stateSource = new EventSource('http://localhost:8080/state')
    trackSource.addEventListener('track', (e) => {
      console.log(e.data)
      const track = JSON.parse(e.data)
      setCurrentTrack(track)
    })
    stateSource.addEventListener('state', (e) => {
      console.log(e.data)
      const state = JSON.parse(e.data)
      setCurrentState(state)
    })
    return () => trackSource.close()
  }, [])
  return (
    <>
      <p>Song</p>
      {currentTrack && (
        <div className='container '>
          <img src={currentTrack.album_cover_url} alt={`${currentTrack.album} cover`} className='album-cover' />
          <p>{currentTrack.title}</p>
          <p>{currentTrack.artist}</p>
          <p>{`${Math.floor(currentTrack.duration_s / 60)}:${currentTrack.duration_s % 60}`}</p>
          <p>{currentTrack.album}</p>
        </div>
      )}
      <p>State</p>
      {currentState && (
        <p>{currentState}</p>
      )}
    </>
  )
}

export default App
