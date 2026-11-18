// Package dsp implements the processed PCM path. The integer bypass never
// constructs a DSP processor.
package dsp

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

type FilterKind string

const (
	Peak      FilterKind = "PK"
	LowShelf  FilterKind = "LS"
	HighShelf FilterKind = "HS"
)

type Filter struct {
	Enabled   bool
	Kind      FilterKind
	Hz, DB, Q float64
}

const MaxFilters = 16
const GraphicBands = 10

func GraphicFrequencies() [GraphicBands]float64 {
	return [GraphicBands]float64{31.25, 62.5, 125, 250, 500, 1000, 2000, 4000, 8000, 16000}
}

// Config is comparable and can be carried by immutable value snapshots.
// A zero value uses source rate, leaves EQ and ReplayGain bypassed and
// quantizes to PCM32. Bypass also disables gain, limiting and dither.
type Config struct {
	Bypass        bool
	Rate          int
	PreampDB      float64
	Filters       [MaxFilters]Filter
	ReplayGain    string
	AlbumFallback bool
	Limit         bool
	Dither        bool
	SwapChannels  bool
	Balance       float64 // -1 leaves left only, +1 leaves right only
	Crossfeed     float64 // opposite-channel contribution at low frequencies
	Graphic       [GraphicBands]float64
}

type Gain struct {
	DB, Peak float64
	Present  bool
}

type Gains struct{ Track, Album Gain }

func (c Config) Validate(rate int) error {
	switch c.Rate {
	case 0, 44100, 48000, 88200, 96000, 176400, 192000:
	default:
		return errors.New("DSP: unsupported fixed output rate")
	}
	switch c.ReplayGain {
	case "", "off", "track", "album":
	default:
		return errors.New("DSP: invalid ReplayGain mode")
	}
	if !between(c.PreampDB, -30, 18) {
		return errors.New("DSP: preamp must be between -30 and +18 dB")
	}
	if !between(c.Balance, -1, 1) || !between(c.Crossfeed, 0, .5) {
		return errors.New("DSP: invalid balance or crossfeed")
	}
	for i, f := range c.Filters {
		if f == (Filter{}) {
			continue
		}
		if !validFilter(f) {
			return fmt.Errorf("DSP: invalid filter %d", i+1)
		}
		if f.Enabled && !c.Bypass && rate > 0 {
			if _, err := coefficients(f, rate); err != nil {
				return fmt.Errorf("DSP: filter %d: %w", i+1, err)
			}
		}
	}
	for i, hz := range GraphicFrequencies() {
		gain := c.Graphic[i]
		if !between(gain, -12, 12) || (gain != 0 && !c.Bypass && rate > 0 && hz >= float64(rate)/2) {
			return fmt.Errorf("DSP: graphic EQ band %d must be within ±12 dB and below Nyquist", i+1)
		}
	}
	return nil
}

func (c Config) eachEQ(yield func(Filter)) {
	for _, f := range c.Filters {
		if f.Enabled {
			yield(f)
		}
	}
	for i, hz := range GraphicFrequencies() {
		if gain := c.Graphic[i]; gain != 0 {
			yield(Filter{Enabled: true, Kind: Peak, Hz: hz, DB: gain, Q: math.Sqrt2})
		}
	}
}

func validFilter(f Filter) bool {
	return (f.Kind == Peak || f.Kind == LowShelf || f.Kind == HighShelf) &&
		between(f.Hz, 10, 96000) && between(f.DB, -24, 24) && between(f.Q, .1, 20)
}

func between(v, lo, hi float64) bool { return v >= lo && v <= hi }
func db(v float64) float64           { return math.Pow(10, v/20) }

// TrackGain selects one tag. A missing album tag applies unity unless track
// fallback was selected. Known source peak provides pre-EQ headroom.
func (c Config) TrackGain(g Gains) float64 {
	if c.Bypass {
		return 1
	}
	var selected Gain
	switch c.ReplayGain {
	case "track":
		selected = g.Track
	case "album":
		selected = g.Album
		if !selected.Present && c.AlbumFallback {
			selected = g.Track
		}
	}
	gain := db(c.PreampDB)
	if selected.Present && between(selected.DB, -60, 60) {
		gain *= db(selected.DB)
		if between(selected.Peak, 1e-9, 100) {
			gain = min(gain, 1/selected.Peak)
		}
	}
	return gain
}

// ParseAutoEQ accepts Equalizer APO parametric entries used by AutoEQ.
// The caller retains the previous preset on any error.
func ParseAutoEQ(input string, base Config) (Config, error) {
	base.Filters, base.PreampDB = [MaxFilters]Filter{}, 0
	s := bufio.NewScanner(strings.NewReader(strings.TrimPrefix(input, "\ufeff")))
	var seen [MaxFilters]bool
	var preamp bool
	for line := 1; s.Scan(); line++ {
		v := strings.TrimSpace(strings.SplitN(strings.SplitN(s.Text(), "#", 2)[0], ";", 2)[0])
		if v == "" {
			continue
		}
		if err := parseLine(v, &base, &seen, &preamp); err != nil {
			return Config{}, fmt.Errorf("AutoEQ line %d: %w", line, err)
		}
	}
	if err := s.Err(); err != nil {
		return Config{}, err
	}
	if !preamp && seen == ([MaxFilters]bool{}) {
		return Config{}, errors.New("AutoEQ: no preamp or filters found")
	}
	return base, nil
}

func parseLine(line string, c *Config, seen *[MaxFilters]bool, preamp *bool) error {
	name, value, ok := strings.Cut(line, ":")
	parts := strings.Fields(value)
	if ok && strings.EqualFold(strings.TrimSpace(name), "Preamp") && len(parts) == 2 && strings.EqualFold(parts[1], "dB") && !*preamp {
		v, err := strconv.ParseFloat(parts[0], 64)
		if err == nil && between(v, -30, 18) {
			c.PreampDB, *preamp = v, true
			return nil
		}
		return errors.New("invalid preamp")
	}
	label := strings.Fields(name)
	if !ok || len(label) != 2 || !strings.EqualFold(label[0], "Filter") || len(parts) < 2 {
		return errors.New("expected Preamp or Filter")
	}
	index, err := strconv.Atoi(label[1])
	if err != nil || index < 1 || index > MaxFilters || seen[index-1] {
		return errors.New("invalid or duplicate filter number")
	}
	seen[index-1] = true
	on := strings.EqualFold(parts[0], "ON")
	if !on && !strings.EqualFold(parts[0], "OFF") {
		return errors.New("expected ON or OFF")
	}
	f := Filter{Enabled: on, Kind: FilterKind(strings.ToUpper(parts[1])), Q: .7071067811865476}
	switch f.Kind {
	case "PEQ":
		f.Kind = Peak
	case "LSC":
		f.Kind = LowShelf
	case "HSC":
		f.Kind = HighShelf
	}
	if len(parts) != 8 && len(parts) != 10 ||
		!strings.EqualFold(parts[2], "Fc") || !strings.EqualFold(parts[4], "Hz") ||
		!strings.EqualFold(parts[5], "Gain") || !strings.EqualFold(parts[7], "dB") {
		return errors.New("expected Fc frequency Hz Gain gain dB [Q quality]")
	}
	f.Hz, err = strconv.ParseFloat(parts[3], 64)
	if err != nil {
		return errors.New("invalid frequency")
	}
	f.DB, err = strconv.ParseFloat(parts[6], 64)
	if err != nil {
		return errors.New("invalid gain")
	}
	if len(parts) == 10 && strings.EqualFold(parts[8], "Q") {
		f.Q, err = strconv.ParseFloat(parts[9], 64)
	} else if len(parts) == 10 || f.Kind == Peak {
		return errors.New("expected filter Q")
	}
	if err != nil || !validFilter(f) {
		return errors.New("unsupported filter or parameter range")
	}
	c.Filters[index-1] = f
	return nil
}
