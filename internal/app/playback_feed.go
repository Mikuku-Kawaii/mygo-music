package app

import "mygo-music/internal/music"

// playbackRadioFeed captures the UI registry once. PlaybackController can ask
// for and receive more tracks while the UI is busy; only error presentation is
// posted back to the window. The initial station click keeps radioFeed's API.
func (a *app) playbackRadioFeed(name string) func(func([]music.Track)) {
	source, id, _ := radioParts(name)
	lib := a.libOf(source)
	if lib == nil {
		return func(got func([]music.Track)) { got(nil) }
	}
	src := lib.src
	return func(got func([]music.Track)) {
		var tracks []music.Track
		var err error
		for tries := 0; tries < 3 && len(tracks) == 0; tries++ {
			var more []music.Track
			more, err = src.StationNext(id)
			if err != nil {
				break
			}
			tracks = playable(more)
		}
		got(tracks)
		if err != nil {
			a.post(func() { a.problem(lib, err, T("The station didn't answer")) })
		}
	}
}
