package fmp4

import (
	"io"

	"eaglesong.dev/hls/internal/fmp4/fmp4io"
	"eaglesong.dev/hls/internal/timescale"
	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/utils/bits/pio"
)

type fragmentWithData struct {
	trackFrag *fmp4io.TrackFrag
	packets   []av.Packet
}

func (f *TrackFragmenter) makeFragment() fragmentWithData {
	if len(f.pending) < 2 {
		return fragmentWithData{}
	}
	entryCount := len(f.pending) - 1
	// timescale for first packet
	startTime := f.pending[0].Time
	startDTS := timescale.ToScale(startTime, f.timeScale)
	// build fragment metadata
	defaultFlags := fmp4io.SampleNoDependencies
	if f.codecData.Type().IsVideo() {
		defaultFlags = fmp4io.SampleNonKeyframe
	}
	track := &fmp4io.TrackFrag{
		Header: &fmp4io.TrackFragHeader{
			Flags:   fmp4io.TrackFragDefaultBaseIsMOOF,
			TrackID: f.trackID,
		},
		DecodeTime: &fmp4io.TrackFragDecodeTime{
			Version: 1,
			Time:    startDTS,
		},
		Run: &fmp4io.TrackFragRun{
			Flags:   fmp4io.TrackRunDataOffset,
			Entries: make([]fmp4io.TrackFragRunEntry, entryCount),
		},
	}
	// add samples to the fragment run
	curDTS := startDTS
	for i, pkt := range f.pending[:entryCount] {
		// calculate the absolute DTS of the next sample and use the difference as the duration
		nextTime := f.pending[i+1].Time
		nextDTS := timescale.ToScale(nextTime, f.timeScale)
		entry := fmp4io.TrackFragRunEntry{
			Duration: uint32(nextDTS - curDTS),
			Flags:    defaultFlags,
			Size:     uint32(len(pkt.Data)),
		}
		if pkt.IsKeyFrame {
			entry.Flags = fmp4io.SampleNoDependencies
		}
		if i == 0 {
			// Optimistically use the first sample's fields as defaults.
			// If a later sample has different values, then the default will be cleared and per-sample values will be used for that field.
			track.Header.DefaultDuration = entry.Duration
			track.Header.DefaultSize = entry.Size
			track.Header.DefaultFlags = entry.Flags
			track.Run.FirstSampleFlags = entry.Flags
		} else {
			if entry.Duration != track.Header.DefaultDuration {
				track.Header.DefaultDuration = 0
			}
			if entry.Size != track.Header.DefaultSize {
				track.Header.DefaultSize = 0
			}
			// The first sample's flags can be specified separately if all other samples have the same flags.
			// Thus the default flags come from the second sample.
			if i == 1 {
				track.Header.DefaultFlags = entry.Flags
			} else if entry.Flags != track.Header.DefaultFlags {
				track.Header.DefaultFlags = 0
			}
		}
		if pkt.CompositionTime != 0 {
			// add composition time to entries in this run
			track.Run.Flags |= fmp4io.TrackRunSampleCTS
			relCTS := timescale.Relative(pkt.CompositionTime, f.timeScale)
			if relCTS < 0 {
				// negative composition time needs version 1
				track.Run.Version = 1
			}
			entry.CTS = relCTS
		}
		// log.Printf("%3d %d -> %d = %d  %s -> %s = %s  comp %s %d", f.trackID, curDTS, nextDTS, nextDTS-curDTS, pkt.Time, nextTime, nextTime-pkt.Time, pkt.CompositionTime, entry.CTS)
		curDTS = nextDTS
		track.Run.Entries[i] = entry
	}
	if track.Header.DefaultSize != 0 {
		// all samples are the same size
		track.Header.Flags |= fmp4io.TrackFragDefaultSize
	} else {
		track.Run.Flags |= fmp4io.TrackRunSampleSize
	}
	if track.Header.DefaultDuration != 0 {
		// all samples are the same duration
		track.Header.Flags |= fmp4io.TrackFragDefaultDuration
	} else {
		track.Run.Flags |= fmp4io.TrackRunSampleDuration
	}
	if track.Header.DefaultFlags != 0 {
		// all samples are the same duration
		track.Header.Flags |= fmp4io.TrackFragDefaultFlags
		if track.Run.FirstSampleFlags != track.Header.DefaultFlags {
			// except the first one
			track.Run.Flags |= fmp4io.TrackRunFirstSampleFlags
		}
	} else {
		track.Run.Flags |= fmp4io.TrackRunSampleFlags
	}
	d := fragmentWithData{
		trackFrag: track,
		packets:   f.pending[:entryCount],
	}
	f.pending = []av.Packet{f.pending[entryCount]}
	return d
}

func writeFragment(w io.Writer, tracks []fragmentWithData, seqNum uint32) error {
	// fill out fragment header
	moof := &fmp4io.MovieFrag{
		Header: &fmp4io.MovieFragHeader{
			Seqnum: seqNum,
		},
		Tracks: make([]*fmp4io.TrackFrag, len(tracks)),
	}
	for i, track := range tracks {
		moof.Tracks[i] = track.trackFrag
	}
	// calculate track data offsets relative to the start of the MOOF
	dataBase := moof.Len() + 8 // MOOF plus the MDAT header
	dataOffset := dataBase
	for i, track := range tracks {
		moof.Tracks[i].Run.DataOffset = uint32(dataOffset)
		for _, pkt := range track.packets {
			dataOffset += len(pkt.Data)
		}
	}
	// marshal MOOF and MDAT header
	b := make([]byte, dataBase)
	moof.Marshal(b)
	pio.PutU32BE(b[dataBase-8:], uint32(dataOffset-dataBase+8))
	pio.PutU32BE(b[dataBase-4:], uint32(fmp4io.MDAT))
	if _, err := w.Write(b); err != nil {
		return err
	}
	// write MDAT contents
	for i, track := range tracks {
		moof.Tracks[i].Run.DataOffset = uint32(dataOffset)
		for _, pkt := range track.packets {
			if _, err := w.Write(pkt.Data); err != nil {
				return err
			}
		}
	}
	return nil
}
