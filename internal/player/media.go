package player

import (
	"time"

	"github.com/egoist/mygo"
)

type mediaSession struct {
	off                    func()
	showing                bool
	session, artwork, seek uint64
	changed                uint64
	commands               mygo.MediaCommand
	state                  mygo.PlaybackState
	duration, position     time.Duration
	at                     time.Time
}

type mediaRequest struct {
	command   mygo.MediaCommand
	position  time.Duration
	session   uint64
	configure func(*playbackState)
}

// BindSystemMedia is installed on the main thread for the application's
// player. Headless players and view fixtures do not take media key ownership.
func (p *Player) BindSystemMedia() {
	if p.media.off == nil {
		p.media.off = mygo.NowPlaying.OnCommand(p.mediaCommand)
		p.media.publish(p.controller.latest.Load())
	}
}

func (p *Player) mediaCommand(cmd mygo.MediaCommand, pos time.Duration) {
	if p.media.commands&cmd == 0 {
		return
	}
	c := p.controller
	select {
	case c.media <- mediaRequest{cmd, pos, p.media.session, p.configure(nil)}:
	case <-c.stop:
	default:
		// The platform callback must not hold up playback or window input.
	}
}

func (r mediaRequest) apply(s *playbackState) {
	if s.track == nil {
		return
	}
	r.configure(s)
	switch r.command {
	case mygo.MediaPlay, mygo.MediaPause:
		if (r.command == mygo.MediaPlay) != s.playing {
			s.TogglePlay()
		}
	case mygo.MediaNext:
		s.Next()
	case mygo.MediaPrevious:
		s.Prev()
	case mygo.MediaSeek:
		// A system scrub may arrive while a gapless transition is on its
		// way to the UI. Its position belongs to the displayed track.
		s.syncNativeBoundary()
		if r.session == uint64(s.gen) && !s.Loading() && (s.output.Mode == OutputCompatible ||
			s.err == "" && s.nativeState.Source.ExactSeek) {
			s.Seek(r.position)
		}
	}
}

func (m *mediaSession) publish(s *PlaybackSnapshot) {
	if m.off == nil {
		return
	}
	if s.Current == nil {
		if m.showing {
			mygo.NowPlaying.Clear()
		}
		m.showing, m.commands = false, 0
		return
	}
	n := s.AudioOutput.Native
	state := mygo.PlaybackPaused
	if s.Playing && !s.Loading && s.Ready && (s.AudioOutput.Requested.Mode == OutputCompatible || n.EffectivePlaying) {
		state = mygo.PlaybackPlaying
	}
	cmds := mygo.MediaPlay | mygo.MediaPause
	if len(s.Order) > 0 {
		cmds |= mygo.MediaNext | mygo.MediaPrevious
	}
	if canSeek(s) && s.Ready && !s.Loading && s.Duration > 0 {
		cmds |= mygo.MediaSeek
	}
	now := time.Now()
	seek := n.AppliedGeneration.Seek
	metadata := !m.showing || m.session != s.SessionGeneration || m.artwork != s.ArtworkSeq ||
		m.commands != cmds || m.duration != s.Duration
	if metadata {
		t := s.Current
		mygo.NowPlaying.Set(mygo.NowPlayingInfo{
			Title: t.Name, Artist: t.ArtistNames(), Album: t.Album.Name, Artwork: s.Artwork,
			Duration: s.Duration, Position: s.Position, State: state, Commands: cmds,
		})
	} else {
		predicted := m.position
		if m.state == mygo.PlaybackPlaying {
			predicted += now.Sub(m.at)
			if m.duration > 0 {
				predicted = min(m.duration, predicted)
			}
		}
		drift := s.Position - predicted
		near := drift > -250*time.Millisecond && drift < 250*time.Millisecond
		if state == m.state && seek == m.seek && (near ||
			(s.ChangedSeq == m.changed && state == mygo.PlaybackPlaying && now.Sub(m.at) < time.Second)) {
			return
		}
		mygo.NowPlaying.SetPlayback(state, s.Position)
	}
	m.showing, m.session, m.artwork, m.seek = true, s.SessionGeneration, s.ArtworkSeq, seek
	m.changed = s.ChangedSeq
	m.commands, m.state, m.duration, m.position, m.at = cmds, state, s.Duration, s.Position, now
}

func (m *mediaSession) close() {
	if m.off != nil {
		m.off()
		m.off = nil
		mygo.NowPlaying.Clear()
	}
}
