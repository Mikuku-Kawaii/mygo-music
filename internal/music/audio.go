package music

// AudioProperties is a local header observation, separate from tags and from
// the decoder/output confirmed at playback. It is not part of the plugin wire API.
type AudioProperties struct {
	Known                                    bool   `json:"known,omitempty"`
	Container                                string `json:"container,omitempty"`
	Rate, Channels, ValidBits, ContainerBits int
	Frames                                   int64
	DSDRate                                  int `json:"dsdRate,omitempty"`
	NativePrecision, ExactSeek, Trimmed      bool
	SeekMethod                               string `json:"seekMethod,omitempty"`
}
