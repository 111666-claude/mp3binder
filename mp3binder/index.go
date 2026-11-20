package mp3binder

import (
	"math"
	"time"

	"github.com/crra/id3v2/v2"
	"github.com/dmulholl/mp3lib"
)

const (
	// xingTOCEntries is the fixed size of the Xing/Info table of contents.
	xingTOCEntries = 100
	// xingTOCScale maps a byte position to a TOC entry: entry = offset*256/size.
	xingTOCScale = 256
	// xingTOCFieldOffset is the offset of the TOC relative to the Xing/Info ID.
	xingTOCFieldOffset = 16
	// xingFlagTOC marks the TOC as present in the Xing/Info flags.
	xingFlagTOC = 0x04
	// checkpointInterval is the maximum distance between two sampled positions
	// of the output timeline. It bounds the memory used for the TOC and the
	// error of a coarse seek.
	checkpointInterval = time.Second
	// referenceSamplingRate is the sampling rate declared by the synthetic
	// Xing/Info frame created by mp3lib.NewXingHeader.
	referenceSamplingRate = 44100
)

// ChapterIndexEntry describes the position of one chapter in the bounded file.
//
// StartOffset and EndOffset are zero-based byte offsets from the start of the
// output file (including its ID3v2 tag) to the first byte of the first complete
// audio frame of the chapter and to the first byte after its last frame. They
// are id3v2.IgnoredOffset when the chapter contains no audio or when the offset
// cannot be represented by the 32-bit field defined by the ID3v2 chapter
// specification; players then fall back to StartTime and EndTime.
type ChapterIndexEntry struct {
	Chapter        int
	Title          string
	StartTime      time.Duration
	EndTime        time.Duration
	StartOffset    uint32
	EndOffset      uint32
	Frames         uint32
	Empty          bool
	OffsetOverflow bool
	// TOC is true if the output carries a Xing TOC derived from this index.
	// A TOC is only written for streams whose frames are all MPEG-1 Layer III
	// at 44.1 kHz, the format of the synthetic Xing/Info frame; the chapter
	// offsets themselves are independent of it.
	TOC bool
}

func (j *job) prepareIndex() {
	if !j.indexEnabled {
		return
	}

	n := len(j.inputs)
	j.inputStartTimes = make([]time.Duration, n)
	j.inputStartBytes = make([]int64, n)
	j.inputByteCounts = make([]int64, n)
	j.inputFrameCount = make([]uint32, n)
}

// beginInput records where the audio of an input file starts in the audio-only
// file. It is called before the first byte of the input is read, so the
// recorded position is the first byte a complete frame could be written to.
// The offset includes the placeholder for the Xing/Info frame.
func (j *job) beginInput(index int, offset int64) {
	if !j.indexEnabled {
		return
	}

	j.currentInput = index
	j.inputStartTimes[index] = j.totalDuration
	j.inputStartBytes[index] = offset

	// Anchor the timeline at the beginning of every input. A chapter start is
	// therefore always represented exactly, even if it falls between two
	// periodic samples.
	j.addCheckpoint(j.totalDuration, offset)
}

// addIndexFrame records a frame that is about to be written at the given
// offset of the audio-only file. Only complete frames reach this method;
// mp3lib drops truncated frames and garbage, so the recorded offsets always
// point at the first byte of a complete frame.
func (j *job) addIndexFrame(frame *mp3lib.MP3Frame, offset int64) {
	if !j.indexEnabled {
		return
	}

	if j.totalFrames == 0 {
		j.referenceRate = frame.SamplingRate
		j.referenceVersion = frame.MPEGVersion
		j.referenceLayer = frame.MPEGLayer
	} else if frame.SamplingRate != j.referenceRate ||
		frame.MPEGVersion != j.referenceVersion ||
		frame.MPEGLayer != j.referenceLayer {
		// Any change of the frame format invalidates the single-format
		// assumption behind the Xing/Info frame.
		j.mixedRates = true
	}

	frameStart := j.totalDuration
	if n := len(j.checkpoints); n == 0 || frameStart-j.checkpoints[n-1].time >= checkpointInterval {
		j.addCheckpoint(frameStart, offset)
	}

	j.inputFrameCount[j.currentInput]++
	j.inputByteCounts[j.currentInput] += int64(len(frame.RawBytes))
	j.totalDuration += duration(frame)
	j.totalBytes += int64(len(frame.RawBytes))
	j.totalFrames++
}

// endInput records the end of an input in the output timeline. It is needed
// for inputs whose last frame is followed by nothing (the common case), so
// that the TOC can interpolate up to the very end of the file.
func (j *job) endInput(index int) {
	if !j.indexEnabled {
		return
	}

	j.addCheckpoint(j.totalDuration, j.inputStartBytes[index]+j.inputByteCounts[index])
}

// addCheckpoint samples the timeline. The offset is relative to the audio-only
// stream (including the Xing/Info placeholder); it is converted to the offset
// relative to the first audio frame, which is the origin used by a Xing TOC.
// Samples with the same or an earlier time are dropped so that the sequence is
// strictly monotonic even if an input contains a frame with a shorter duration.
func (j *job) addCheckpoint(at time.Duration, offset int64) {
	offset -= emptyInfoXingFrameSize
	if offset < 0 {
		offset = 0
	}

	if n := len(j.checkpoints); n > 0 {
		last := j.checkpoints[n-1]
		if at < last.time || (at == last.time && offset <= last.offset) {
			return
		}
	}

	j.checkpoints = append(j.checkpoints, indexCheckpoint{time: at, offset: offset})
}

// tocCompatible reports whether the merged audio can be described by a Xing
// TOC. The synthetic Xing/Info frame is always a 44.1 kHz MPEG-1 Layer III
// frame, so a TOC would map its time stamps to the wrong positions for any
// other rate, version or layer. Mixed rates are not representable either.
func (j *job) tocCompatible() bool {
	return j.indexEnabled &&
		j.totalFrames > 0 &&
		!j.mixedRates &&
		j.referenceRate == referenceSamplingRate &&
		j.referenceVersion == mp3lib.MPEGVersion1 &&
		j.referenceLayer == mp3lib.MPEGLayerIII &&
		j.totalBytes > 0 &&
		j.totalBytes <= math.MaxUint32
}

// toc returns the 100 byte Xing TOC for the bounded stream or nil if the
// stream cannot be described by one.
func (j *job) toc() []byte {
	if !j.tocCompatible() {
		return nil
	}

	return buildTOC(j.checkpoints, j.totalDuration, j.totalBytes)
}

// buildTOC converts the sampled output timeline into the normalized byte
// positions expected by the Xing/Info format. Entry i describes the position
// at i/100th of the total duration. The result is monotonically increasing and
// in the range 0..255.
func buildTOC(checkpoints []indexCheckpoint, totalDuration time.Duration, totalBytes int64) []byte {
	if totalBytes <= 0 || totalDuration <= 0 || len(checkpoints) == 0 {
		return nil
	}

	toc := make([]byte, xingTOCEntries)

	var previous int64
	for i := 1; i < xingTOCEntries; i++ {
		target := time.Duration(int64(totalDuration) * int64(i) / int64(xingTOCEntries))
		offset := offsetAtTime(checkpoints, target)

		if offset < 0 {
			offset = 0
		}

		if offset > totalBytes {
			offset = totalBytes
		}

		entry := (offset*xingTOCScale + totalBytes/2) / totalBytes
		if entry > 255 {
			entry = 255
		}

		if entry < previous {
			entry = previous
		}

		toc[i] = byte(entry)
		previous = entry
	}

	return toc
}

// offsetAtTime returns the byte offset at the given time. Positions between
// two samples are interpolated linearly, which is precise enough for the
// coarse positioning the TOC is intended for.
func offsetAtTime(checkpoints []indexCheckpoint, target time.Duration) int64 {
	if len(checkpoints) == 0 || target <= checkpoints[0].time {
		return 0
	}

	low, high := 0, len(checkpoints)-1
	for low < high {
		middle := (low + high + 1) / 2
		if checkpoints[middle].time <= target {
			low = middle
		} else {
			high = middle - 1
		}
	}

	current := checkpoints[low]
	if low+1 >= len(checkpoints) {
		return current.offset
	}

	next := checkpoints[low+1]
	span := int64(next.time - current.time)
	if span <= 0 {
		return current.offset
	}

	delta := next.offset - current.offset
	offset := current.offset + delta*int64(target-current.time)/span
	if offset < current.offset {
		return current.offset
	}

	if offset > next.offset {
		return next.offset
	}

	return offset
}

// chapterOffset converts an absolute byte offset into the 32-bit field used by
// the ID3v2 chapter specification. Values that do not fit are reported as
// ignored so that players fall back to the chapter time.
func chapterOffset(offset int64) uint32 {
	if offset < 0 || offset >= int64(id3v2.IgnoredOffset) {
		return id3v2.IgnoredOffset
	}

	return uint32(offset)
}

// writeChapterIndex rewrites the chapter frames with byte offsets of the final
// output file and reports the index through the visitor. The tag size is known
// at this point because all metadata frames (including the chapters) have been
// added; the offsets themselves have a fixed width and do not change the size.
func writeChapterIndex() (stage, string, jobProcessor) {
	return stageWriteIndex, "building chapter index", func(j *job) error {
		if !j.indexEnabled || len(j.chapters) == 0 {
			return nil
		}

		tagSize := int64(j.tag.Size())
		// The same condition that made the bind stage write a TOC.
		tocWritten := j.tocCompatible()

		for i := range j.chapters {
			chapter := j.chapters[i]
			input := j.chapterInputs[i]

			entry := ChapterIndexEntry{
				Chapter:   i + 1,
				Title:     chapterTitle(chapter),
				StartTime: chapter.StartTime,
				EndTime:   chapter.EndTime,
				Frames:    j.inputFrameCount[input],
				TOC:       tocWritten,
			}

			if entry.Frames == 0 {
				// An empty chapter has no first frame. Point players at the
				// time instead of inventing a byte offset.
				chapter.StartOffset = id3v2.IgnoredOffset
				chapter.EndOffset = id3v2.IgnoredOffset
				entry.Empty = true
			} else {
				// inputStartBytes is relative to the audio-only stream and
				// includes the Xing/Info placeholder. The final file starts
				// with the ID3v2 tag, so add its size to get the absolute
				// offset the chapter specification asks for.
				start := tagSize + j.inputStartBytes[input]
				end := start + j.inputByteCounts[input]

				chapter.StartOffset = chapterOffset(start)
				chapter.EndOffset = chapterOffset(end)
				entry.OffsetOverflow = chapter.StartOffset == id3v2.IgnoredOffset ||
					chapter.EndOffset == id3v2.IgnoredOffset
			}

			j.chapters[i] = chapter
			j.tag.AddChapterFrame(chapter)

			entry.StartOffset = chapter.StartOffset
			entry.EndOffset = chapter.EndOffset
			j.indexVisitor(entry)
		}

		return nil
	}
}

func chapterTitle(chapter id3v2.ChapterFrame) string {
	if chapter.Title == nil {
		return ""
	}

	return chapter.Title.Text
}

// writeTOC stores the normalized positions in the Xing/Info header and marks
// the TOC as present. A TOC that could not be built leaves the header exactly
// as mp3lib created it.
func writeTOC(header *mp3lib.MP3Frame, toc []byte) {
	if len(toc) != xingTOCEntries {
		return
	}

	offset := 4 + getSideInfoSize(header)

	// Only a Xing or Info frame has the layout the TOC is defined for.
	if id := string(header.RawBytes[offset : offset+4]); id != "Xing" && id != "Info" {
		return
	}

	tocOffset := offset + xingTOCFieldOffset
	if tocOffset+xingTOCEntries > len(header.RawBytes) {
		return
	}

	header.RawBytes[offset+7] |= xingFlagTOC
	copy(header.RawBytes[tocOffset:tocOffset+xingTOCEntries], toc)
}
