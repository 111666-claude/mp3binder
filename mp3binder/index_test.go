package mp3binder

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/crra/id3v2/v2"
	"github.com/dmulholl/mp3lib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	mp3SampleRate44 = 44100
)

var (
	v1l3Bitrates = []int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	v2l3Bitrates = []int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
)

// memFile is an in-memory io.ReadWriteSeeker. The binder needs a seekable
// audio-only file and a seekable output file.
type memFile struct {
	data []byte
	off  int64
}

func (m *memFile) Read(p []byte) (int, error) {
	if m.off >= int64(len(m.data)) {
		return 0, io.EOF
	}

	n := copy(p, m.data[m.off:])
	m.off += int64(n)

	return n, nil
}

func (m *memFile) Write(p []byte) (int, error) {
	if m.off > int64(len(m.data)) {
		m.data = append(m.data, make([]byte, m.off-int64(len(m.data)))...)
	}

	if end := m.off + int64(len(p)); end > int64(len(m.data)) {
		m.data = append(m.data, make([]byte, end-int64(len(m.data)))...)
	}

	copy(m.data[m.off:], p)
	m.off += int64(len(p))

	return len(p), nil
}

func (m *memFile) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = m.off + offset
	case io.SeekEnd:
		next = int64(len(m.data)) + offset
	default:
		return 0, errors.New("invalid whence")
	}

	if next < 0 {
		return 0, errors.New("negative position")
	}

	m.off = next

	return next, nil
}

type nopTagResolver struct{}

func (nopTagResolver) DescriptionFor(string) (string, error) { return "", nil }

// mp3Frame describes one synthetic MPEG audio frame.
type mp3Frame struct {
	// bitrate index, 1..14
	bitrateIndex int
	// true for a MPEG-2 (LSF) frame, false for MPEG-1
	mpeg2 bool
	// sampling rate index, 0..2
	sampleRateIndex int
	padding         bool
	// marker is written into the payload to identify the frame later
	marker byte
}

func (f mp3Frame) sampleRate() int {
	rates := []int{44100, 48000, 32000}
	if f.mpeg2 {
		rates = []int{22050, 24000, 16000}
	}

	return rates[f.sampleRateIndex]
}

func (f mp3Frame) bitrate() int {
	table := v1l3Bitrates
	if f.mpeg2 {
		table = v2l3Bitrates
	}

	return table[f.bitrateIndex] * 1000
}

func (f mp3Frame) bytes() []byte {
	version := byte(0xFB) // MPEG-1, Layer III, no CRC
	if f.mpeg2 {
		version = 0xF3 // MPEG-2, Layer III, no CRC
	}

	header := []byte{
		0xFF,
		version,
		byte(f.bitrateIndex<<4) | byte(f.sampleRateIndex<<2),
		0xC0, // stereo, no mode extension, no emphasis
	}

	if f.padding {
		header[2] |= 0x02
	}

	frameLength := frameLength(f.mpeg2, f.bitrate(), f.sampleRate(), f.padding)
	raw := make([]byte, frameLength)
	copy(raw, header)
	for i := 4; i < len(raw); i++ {
		raw[i] = f.marker
	}

	return raw
}

func frameLength(mpeg2 bool, bitrate, sampleRate int, padding bool) int {
	samples := 1152
	if mpeg2 {
		samples = 576
	}

	length := (samples / 8) * bitrate / sampleRate
	if padding {
		length++
	}

	return length
}

func (f mp3Frame) duration() time.Duration {
	samples := 1152
	if f.mpeg2 {
		samples = 576
	}

	return time.Duration(int64(float64(time.Millisecond) * (1000 / float64(f.sampleRate())) * float64(samples)))
}

// audio builds a stream of frames with the given bitrate indices. Rate and
// version are constant, so the stream has a single frame length.
func audio(bitrateIndices []int, mpeg2 bool, sampleRateIndex int, padding bool) []byte {
	buffer := &bytes.Buffer{}

	for i, index := range bitrateIndices {
		frame := mp3Frame{
			bitrateIndex:    index,
			mpeg2:           mpeg2,
			sampleRateIndex: sampleRateIndex,
			padding:         padding,
			marker:          byte(i),
		}
		buffer.Write(frame.bytes())
	}

	return buffer.Bytes()
}

// frames returns a stream of the given number of frames all with the same
// bitrate index.
func frames(count, bitrateIndex int) []byte {
	indices := make([]int, count)
	for i := range indices {
		indices[i] = bitrateIndex
	}

	return audio(indices, false, 0, false)
}

func vbrFrames(count int) []byte {
	indices := make([]int, count)
	for i := range indices {
		indices[i] = 1 + (i*7)%14 // pseudo-random but deterministic
	}

	return audio(indices, false, 0, false)
}

func id3Prefix(title string) []byte {
	tag := id3v2.NewEmptyTag()
	if title != "" {
		tag.AddFrame(tagTitle, &id3v2.TextFrame{Encoding: id3v2.EncodingUTF8, Text: title})
	}

	buffer := &bytes.Buffer{}
	if _, err := tag.WriteTo(buffer); err != nil {
		panic(err)
	}

	return buffer.Bytes()
}

type chapterIndex struct {
	entries []ChapterIndexEntry
	chaps   []id3v2.ChapterFrame
}

func bind(t *testing.T, inputs [][]byte, options ...Option) ([]byte, chapterIndex) {
	t.Helper()

	readers := make([]io.Reader, len(inputs))
	for i := range inputs {
		readers[i] = bytes.NewReader(inputs[i])
	}

	output := &memFile{}
	audioOnly := &memFile{}

	var collected chapterIndex
	allOptions := []Option{ChapterIndexVisitor(func(entry ChapterIndexEntry) {
		collected.entries = append(collected.entries, entry)
	})}
	allOptions = append(allOptions, options...)

	err := Bind(context.Background(), nopTagResolver{}, output, audioOnly, readers, allOptions...)
	require.NoError(t, err)

	tag, err := id3v2.ParseReader(bytes.NewReader(output.data), id3v2.Options{Parse: true})
	require.NoError(t, err)

	for _, f := range tag.GetFrames("CHAP") {
		chapter, ok := f.(id3v2.ChapterFrame)
		require.True(t, ok)
		collected.chaps = append(collected.chaps, chapter)
	}

	return output.data, collected
}

func chaptersFor(titles ...string) Option {
	return Chapters(func(index, chapterIndex int) (bool, string) {
		if index >= len(titles) {
			return false, ""
		}

		return true, titles[index]
	})
}

const (
	tagLengthFieldSize  = 10
	tagLengthFieldMask  = 0x7f
	tagLengthFieldShift = 7
)

// tagSizeFromOutput returns the size of the leading ID3v2 tag as declared by
// its header. This is the number of bytes actually written, which is what the
// chapter offsets are based on. (Tag.Size() cannot be used on a parsed tag:
// the parser attaches an empty Description subframe to chapter frames, so a
// re-computed size is larger than the bytes on disk.)
func tagSizeFromOutput(t *testing.T, data []byte) int64 {
	t.Helper()

	// Without chapters the binder writes no tag at all (legacy behaviour).
	if len(data) < 10 || !bytes.Equal(data[:3], []byte("ID3")) {
		return 0
	}

	size := int64(data[6]&tagLengthFieldMask)<<(3*tagLengthFieldShift) |
		int64(data[7]&tagLengthFieldMask)<<(2*tagLengthFieldShift) |
		int64(data[8]&tagLengthFieldMask)<<tagLengthFieldShift |
		int64(data[9]&tagLengthFieldMask)

	return tagLengthFieldSize + size
}

// parseFramesIn parses MPEG frames from the given absolute byte range.
func parseFramesIn(t *testing.T, data []byte, offset, end int64) []*mp3lib.MP3Frame {
	t.Helper()

	require.Less(t, offset, int64(len(data)))
	require.LessOrEqual(t, end, int64(len(data)))

	reader := bytes.NewReader(data[offset:end])
	var parsed []*mp3lib.MP3Frame
	for {
		obj := mp3lib.NextObject(reader)
		if obj == nil {
			return parsed
		}

		if frame, ok := obj.(*mp3lib.MP3Frame); ok {
			parsed = append(parsed, frame)
		}
	}
}

func assertFrameSync(t *testing.T, data []byte, offset int64) {
	t.Helper()

	require.Less(t, offset+1, int64(len(data)))
	assert.Equal(t, byte(0xFF), data[offset], "offset %d does not point at a frame sync", offset)
	assert.Equal(t, byte(0xE0), data[offset+1]&0xE0, "offset %d does not point at a frame sync", offset)
}

// xingHeader locates the Xing/Info frame of the output.
func xingHeader(t *testing.T, data []byte) (*mp3lib.MP3Frame, int) {
	t.Helper()

	tagSize := tagSizeFromOutput(t, data)
	frame := mp3lib.NextFrame(bytes.NewReader(data[tagSize:]))
	require.NotNil(t, frame)
	require.True(t, mp3lib.IsXingHeader(frame), "expected a Xing/Info header")

	return frame, 4 + getSideInfoSize(frame)
}

func TestChapterOffsetsPointAtFirstFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		inputs [][]byte
	}{
		{
			name:   "CBR",
			inputs: [][]byte{frames(10, 9), frames(12, 9), frames(8, 9)},
		},
		{
			name:   "VBR",
			inputs: [][]byte{vbrFrames(10), vbrFrames(12), vbrFrames(8)},
		},
		{
			name: "mixed CBR and VBR",
			inputs: [][]byte{
				frames(6, 9),
				vbrFrames(7),
				frames(9, 5),
			},
		},
		{
			name: "variable frame length within a chapter",
			inputs: [][]byte{
				audio([]int{9, 9, 12, 4, 9, 9, 14}, false, 0, false),
				audio([]int{9, 9, 12, 4, 9, 9, 14}, false, 0, true),
				frames(5, 9),
			},
		},
		{
			name: "chapter with ID3 prefix",
			inputs: [][]byte{
				append(id3Prefix("one"), frames(7, 9)...),
				append(id3Prefix("two"), frames(7, 9)...),
				append(id3Prefix("three"), frames(7, 9)...),
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			titles := make([]string, len(tt.inputs))
			for i := range titles {
				titles[i] = "title"
			}

			output, index := bind(t, tt.inputs, chaptersFor(titles...))

			require.Len(t, index.entries, len(tt.inputs))
			require.Len(t, index.chaps, len(tt.inputs))

			var previousEnd uint32
			for i, entry := range index.entries {
				assert.False(t, entry.Empty || entry.OffsetOverflow)
				assertFrameSync(t, output, int64(entry.StartOffset))

				// The first frame of the chapter carries this chapter's marker.
				chapterFrames := parseFramesIn(t, output, int64(entry.StartOffset), int64(entry.EndOffset))
				assert.NotEmpty(t, chapterFrames)
				assert.Equal(t, byte(0), chapterFrames[0].RawBytes[4])

				assert.GreaterOrEqual(t, entry.StartOffset, previousEnd)
				previousEnd = entry.EndOffset

				// Chapter times are stored in milliseconds.
				assert.Equal(t, entry.StartTime.Truncate(time.Millisecond), index.chaps[i].StartTime)
				assert.Equal(t, entry.EndTime.Truncate(time.Millisecond), index.chaps[i].EndTime)
				assert.Equal(t, entry.StartOffset, index.chaps[i].StartOffset)
				assert.Equal(t, entry.EndOffset, index.chaps[i].EndOffset)
			}
		})
	}
}

func TestChapterOffsetsMatchFramesAndDurations(t *testing.T) {
	t.Parallel()

	inputs := [][]byte{
		audio([]int{9, 12, 4, 9, 12, 4}, false, 0, false),
		audio([]int{4, 14, 4, 9}, false, 0, true),
		vbrFrames(11),
	}

	output, index := bind(t, inputs, chaptersFor("a", "b", "c"))

	require.Len(t, index.entries, 3)

	for _, entry := range index.entries {
		chapterFrames := parseFramesIn(t, output, int64(entry.StartOffset), int64(entry.EndOffset))
		require.NotEmpty(t, chapterFrames)

		var sum time.Duration
		var sizes int64
		for _, frame := range chapterFrames {
			sum += duration(frame)
			sizes += int64(len(frame.RawBytes))
		}

		// The byte range must cover exactly the frames of the chapter.
		assert.Equal(t, int64(entry.EndOffset-entry.StartOffset), sizes)
		assert.Equal(t, entry.EndTime-entry.StartTime, sum)
	}

	// Seeking to the end offset must reach the next chapter (or EOF).
	for i, entry := range index.entries {
		end := int64(entry.EndOffset)
		if i+1 < len(index.entries) {
			assert.Equal(t, end, int64(index.entries[i+1].StartOffset))
			continue
		}

		assert.Equal(t, end, int64(len(output)))
	}
}

// rawFrames returns the raw bytes of every complete frame in a stream. ID3
// tags, garbage and a leading Xing/Info frame are skipped like the binder does.
func rawFrames(t *testing.T, data []byte) [][]byte {
	t.Helper()

	reader := bytes.NewReader(data)
	var frames [][]byte
	for i := 0; ; i++ {
		obj := mp3lib.NextObject(reader)
		if obj == nil {
			return frames
		}

		frame, ok := obj.(*mp3lib.MP3Frame)
		if !ok {
			continue
		}

		if i == 0 && (mp3lib.IsXingHeader(frame) || mp3lib.IsVbriHeader(frame)) {
			continue
		}

		frames = append(frames, append([]byte{}, frame.RawBytes...))
	}
}

// TestChapterByteRangesAreBitExact proves that the recorded range of every
// chapter consists of exactly the complete frames of its input, in order.
func TestChapterByteRangesAreBitExact(t *testing.T) {
	t.Parallel()

	garbage := []byte{0x00, 0xFF, 0xFB, 0x90, 0x00, 0x11, 0x22} // contains a fake sync

	inputs := [][]byte{
		frames(7, 9),
		append(id3Prefix("prefixed"), vbrFrames(11)...),
		append(append([]byte{}, garbage...), audio([]int{5, 5, 12, 3, 14}, false, 0, true)...),
		append(append([]byte{}, frames(4, 6)...), frames(1, 6)[:120]...), // truncated tail
	}

	output, index := bind(t, inputs, chaptersFor("one", "two", "three", "four"))
	require.Len(t, index.entries, len(inputs))

	for i, entry := range index.entries {
		expected := rawFrames(t, inputs[i])
		require.NotEmpty(t, expected)

		actual := rawFrames(t, output[entry.StartOffset:entry.EndOffset])
		require.Len(t, actual, len(expected), "chapter %d frame count", entry.Chapter)

		for k := range expected {
			assert.Equal(t, expected[k], actual[k], "chapter %d frame %d differs", entry.Chapter, k)
		}

		assert.Equal(t, uint32(len(expected)), entry.Frames)
	}
}

func TestTotalDurationMatchesChapterTimeline(t *testing.T) {
	t.Parallel()

	const frameCount = 40

	output, index := bind(t, [][]byte{frames(frameCount, 9)}, chaptersFor("only"))

	require.Len(t, index.entries, 1)

	all := parseFramesIn(t, output, int64(index.entries[0].StartOffset), int64(len(output)))
	require.Len(t, all, frameCount)

	var total time.Duration
	for _, frame := range all {
		total += duration(frame)
	}

	assert.Equal(t, total, index.entries[0].EndTime-index.entries[0].StartTime)
}

func TestEmptyChapterFallsBackToTime(t *testing.T) {
	t.Parallel()

	inputs := [][]byte{
		frames(5, 9),
		{}, // empty file
		frames(5, 9),
	}

	output, index := bind(t, inputs, chaptersFor("one", "empty", "two"))

	require.Len(t, index.entries, 3)

	assert.False(t, index.entries[0].Empty)
	assert.True(t, index.entries[1].Empty)
	assert.False(t, index.entries[2].Empty)

	assert.Equal(t, uint32(id3v2.IgnoredOffset), index.chaps[1].StartOffset)
	assert.Equal(t, uint32(id3v2.IgnoredOffset), index.chaps[1].EndOffset)
	assert.Equal(t, index.chaps[1].StartTime, index.chaps[1].EndTime)

	// The empty chapter must not disturb the following chapter.
	assertFrameSync(t, output, int64(index.entries[2].StartOffset))
	assert.Equal(t, index.entries[0].EndOffset, index.entries[2].StartOffset)
	assert.Equal(t, index.entries[0].EndTime, index.entries[2].StartTime)
}

func TestTruncatedFrameIsNotIndexed(t *testing.T) {
	t.Parallel()

	complete := frames(6, 9)

	// The second input declares a complete frame but ends in the middle of it.
	inputs := [][]byte{
		complete,
		append(append([]byte{}, frames(4, 9)...), frames(1, 9)[:100]...),
		frames(4, 9),
	}

	output, index := bind(t, inputs, chaptersFor("one", "two", "three"))

	require.Len(t, index.entries, 3)
	assertFrameSync(t, output, int64(index.entries[1].StartOffset))
	assertFrameSync(t, output, int64(index.entries[2].StartOffset))

	// The truncated frame must not be part of the output at all.
	second := parseFramesIn(t, output, int64(index.entries[1].StartOffset), int64(index.entries[1].EndOffset))
	require.Len(t, second, 4)
	assert.Equal(t, int64(index.entries[1].EndOffset-index.entries[1].StartOffset), int64(len(frames(4, 9))))
	assert.False(t, index.entries[1].Empty)

	// The chapter still describes only complete frames.
	assert.Equal(t, index.entries[1].EndOffset, index.entries[2].StartOffset)
}

func TestChapterBoundaryWithLeadingGarbage(t *testing.T) {
	t.Parallel()

	// Garbage that does not contain a sync word before the first frame.
	garbage := []byte{0x00, 0x11, 0x22, 0x33, 0x44}
	inputs := [][]byte{
		frames(4, 9),
		append(garbage, frames(4, 9)...),
		frames(4, 9),
	}

	output, index := bind(t, inputs, chaptersFor("one", "two", "three"))

	require.Len(t, index.entries, 3)

	// The offset points at the first complete frame, not at the garbage.
	assertFrameSync(t, output, int64(index.entries[1].StartOffset))
	assert.Equal(t, int64(index.entries[0].EndOffset), int64(index.entries[1].StartOffset))
	assertFrameSync(t, output, int64(index.entries[2].StartOffset))
}

func TestDuplicateTitlesKeepDistinctChapters(t *testing.T) {
	t.Parallel()

	inputs := [][]byte{frames(4, 9), frames(4, 9), frames(4, 9)}

	output, index := bind(t, inputs, chaptersFor("Same", "Same", "Same"))

	require.Len(t, index.chaps, 3)

	ids := map[string]struct{}{}
	for _, chapter := range index.chaps {
		assert.Equal(t, "Same", chapter.Title.Text)
		ids[chapter.ElementID] = struct{}{}
	}
	assert.Len(t, ids, 3)

	// The table of contents lists all three chapters.
	tag, err := id3v2.ParseReader(bytes.NewReader(output), id3v2.Options{Parse: true})
	require.NoError(t, err)
	tocs := tag.GetFrames("CTOC")
	require.Len(t, tocs, 1)
	assert.Len(t, tocs[0].(id3v2.ChapterTocFrame).ChapterIds, 3)
}

func TestSkippedChapterKeepsTimeline(t *testing.T) {
	t.Parallel()

	inputs := [][]byte{frames(4, 9), frames(6, 9), frames(4, 9)}

	// The second input is bound but not a chapter (e.g. an interlace file).
	resolver := Chapters(func(index, chapterIndex int) (bool, string) {
		return index != 1, "chapter"
	})

	output, index := bind(t, inputs, resolver)

	require.Len(t, index.entries, 2)

	assert.Equal(t, time.Duration(0), index.entries[0].StartTime)

	// The timeline continues through the skipped input, so the second
	// chapter starts one skipped input later.
	skippedDuration := index.entries[1].StartTime - index.entries[0].EndTime
	require.Greater(t, skippedDuration, time.Duration(0))

	// The second chapter starts after the skipped input.
	first := parseFramesIn(t, output, int64(index.entries[0].StartOffset), int64(index.entries[0].EndOffset))
	require.Len(t, first, 4)

	second := parseFramesIn(t, output, int64(index.entries[1].StartOffset), int64(index.entries[1].EndOffset))
	require.Len(t, second, 4)

	assert.Equal(t, index.entries[0].EndOffset+uint32(len(frames(6, 9))), index.entries[1].StartOffset)
}

func TestNoChaptersPreservesLegacyHeader(t *testing.T) {
	t.Parallel()

	inputs := [][]byte{frames(5, 9), frames(5, 9)}
	output, index := bind(t, inputs)

	assert.Empty(t, index.entries)
	assert.Empty(t, index.chaps)

	header, offset := xingHeader(t, output)
	flags := header.RawBytes[offset+7]
	assert.Zero(t, flags&xingFlagTOC, "legacy mode must not write a TOC")

	for _, b := range header.RawBytes[offset+xingTOCFieldOffset : offset+xingTOCFieldOffset+xingTOCEntries] {
		assert.Zero(t, b, "legacy mode must leave the TOC area empty")
	}

	tag, err := id3v2.ParseReader(bytes.NewReader(output), id3v2.Options{Parse: true})
	require.NoError(t, err)
	assert.Empty(t, tag.GetFrames("CHAP"))
}

func TestXingTOCPresentAndMonotonic(t *testing.T) {
	t.Parallel()

	inputs := [][]byte{vbrFrames(30), vbrFrames(40), vbrFrames(20)}

	output, _ := bind(t, inputs, chaptersFor("one", "two", "three"))

	header, offset := xingHeader(t, output)
	flags := header.RawBytes[offset+7]
	require.NotZero(t, flags&xingFlagTOC, "TOC flag expected")

	toc := header.RawBytes[offset+xingTOCFieldOffset : offset+xingTOCFieldOffset+xingTOCEntries]
	assert.Zero(t, toc[0])
	for i := 1; i < len(toc); i++ {
		assert.LessOrEqual(t, toc[i-1], toc[i], "TOC must be monotonic at entry %d", i)
	}

	totalBytes := int64(binary.BigEndian.Uint32(header.RawBytes[offset+12 : offset+16]))
	tagSize := tagSizeFromOutput(t, output)
	assert.Equal(t, int64(len(output))-(tagSize+emptyInfoXingFrameSize), totalBytes)
}

func TestXingTOCPositionsAreCoarseAccurate(t *testing.T) {
	t.Parallel()

	// A VBR stream with a strongly changing bitrate makes interpolation errors
	// visible; the samples are ~1 second apart.
	indices := make([]int, 0, 90)
	for i := 0; i < 30; i++ {
		indices = append(indices, 4)
	}
	for i := 0; i < 30; i++ {
		indices = append(indices, 14)
	}
	for i := 0; i < 30; i++ {
		indices = append(indices, 9)
	}

	inputs := [][]byte{
		audio(indices[:30], false, 0, false),
		audio(indices[30:60], false, 0, false),
		audio(indices[60:], false, 0, false),
	}

	output, _ := bind(t, inputs, chaptersFor("one", "two", "three"))

	header, offset := xingHeader(t, output)
	toc := header.RawBytes[offset+xingTOCFieldOffset : offset+xingTOCFieldOffset+xingTOCEntries]

	totalBytes := int64(binary.BigEndian.Uint32(header.RawBytes[offset+12 : offset+16]))
	totalFrames := int64(binary.BigEndian.Uint32(header.RawBytes[offset+8 : offset+12]))
	require.NotZero(t, totalFrames)

	tagSize := tagSizeFromOutput(t, output)
	audioStart := tagSize + emptyInfoXingFrameSize

	// Walk the real output and build the true timeline of the audio data.
	type position struct {
		offset int64
		at     time.Duration
	}

	var timeline []position
	var totalDuration time.Duration

	for cursor := audioStart; cursor < int64(len(output)); {
		reader := bytes.NewReader(output[cursor:])
		obj := mp3lib.NextObject(reader)
		if obj == nil {
			break
		}

		frame, ok := obj.(*mp3lib.MP3Frame)
		if !ok {
			continue
		}

		timeline = append(timeline, position{offset: cursor - audioStart, at: totalDuration})
		totalDuration += duration(frame)
		cursor += int64(len(frame.RawBytes))
	}

	require.NotEmpty(t, timeline)

	frameDuration := duration(&mp3lib.MP3Frame{
		MPEGVersion:  mp3lib.MPEGVersion1,
		MPEGLayer:    mp3lib.MPEGLayerIII,
		SamplingRate: mp3SampleRate44,
		SampleCount:  1152,
	})

	for i := 1; i < xingTOCEntries; i++ {
		tocOffset := int64(toc[i]) * totalBytes / xingTOCScale
		require.Less(t, tocOffset, totalBytes)

		// Find the first frame at or after the coarse position.
		index := sort.Search(len(timeline), func(k int) bool {
			return timeline[k].offset >= tocOffset
		})

		// The last entry may describe the very end of the stream; a player
		// then resyncs backwards to the last frame.
		if index == len(timeline) {
			index = len(timeline) - 1
		}

		actual := timeline[index].at

		// Frame-aligned positions are expected to be within a few frames of
		// the time the entry represents.
		target := time.Duration(int64(totalDuration) * int64(i) / xingTOCEntries)
		tolerance := totalDuration / 50 // 2%
		if tolerance < 4*frameDuration {
			tolerance = 4 * frameDuration
		}

		assert.InDelta(t, float64(target), float64(actual), float64(tolerance),
			"entry %d is too far from its time", i)
	}
}

// TestPlayerSeekFromChapterIndex simulates the two ways a player locates a
// chapter: directly through the byte offset in the CHAP frame and coarsely
// through the Xing TOC. In both cases the frames that are played from the
// chapter start must be exactly the frames of that input, and the time until
// the next chapter must match the chapter duration.
func TestPlayerSeekFromChapterIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		inputs [][]byte
	}{
		{
			name: "VBR",
			inputs: [][]byte{
				vbrFrames(40),
				vbrFrames(55),
				vbrFrames(30),
			},
		},
		{
			name: "CBR",
			inputs: [][]byte{
				frames(40, 9),
				frames(55, 7),
				frames(30, 12),
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			titles := []string{"first", "second", "third"}
			output, index := bind(t, tt.inputs, chaptersFor(titles...))
			require.Len(t, index.entries, len(tt.inputs))

			header, offset := xingHeader(t, output)
			toc := header.RawBytes[offset+xingTOCFieldOffset : offset+xingTOCFieldOffset+xingTOCEntries]
			totalBytes := int64(binary.BigEndian.Uint32(header.RawBytes[offset+12 : offset+16]))
			tagSize := tagSizeFromOutput(t, output)
			audioStart := tagSize + emptyInfoXingFrameSize

			for i, entry := range index.entries {
				// Direct seek: the offset must be the first frame of the chapter.
				played := parseFramesIn(t, output, int64(entry.StartOffset), int64(entry.EndOffset))
				require.NotEmpty(t, played)

				var playedDuration time.Duration
				for _, frame := range played {
					playedDuration += duration(frame)
				}

				assert.Equal(t, entry.EndTime-entry.StartTime, playedDuration,
					"chapter %d: duration of the frames at the offset differs", entry.Chapter)

				if i == len(index.entries)-1 {
					continue
				}

				// Coarse seek through the TOC to the chapter time.
				position := 0.0
				if index.entries[len(index.entries)-1].EndTime > 0 {
					position = float64(entry.StartTime) / float64(index.entries[len(index.entries)-1].EndTime)
				}

				tocIndex := int(position * xingTOCEntries)
				if tocIndex < 0 {
					tocIndex = 0
				}

				if tocIndex >= xingTOCEntries {
					tocIndex = xingTOCEntries - 1
				}

				coarse := audioStart + int64(toc[tocIndex])*totalBytes/xingTOCScale
				resynced := mp3lib.NextFrame(bytes.NewReader(output[coarse:]))
				require.NotNil(t, resynced, "chapter %d: no frame after the TOC position", entry.Chapter)
				coarseFrame := bytes.Index(output[coarse:], resynced.RawBytes)

				// Walk frames from the resynced position to the chapter start.
				cursor := coarse + int64(coarseFrame)
				var walked time.Duration

				for cursor < int64(entry.StartOffset) {
					frame := mp3lib.NextFrame(bytes.NewReader(output[cursor:]))
					require.NotNil(t, frame)
					walked += duration(frame)
					cursor += int64(len(frame.RawBytes))
				}

				assert.Less(t, walked, 3*time.Second,
					"chapter %d: resyncing from the TOC takes too long", entry.Chapter)
			}
		})
	}
}

func TestTOCDisabledForIncompatibleSampleRate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		inputs [][]byte
	}{
		{
			name:   "MPEG-2 22.05 kHz",
			inputs: [][]byte{audio([]int{9, 9, 9, 9}, true, 0, false), audio([]int{9, 9}, true, 0, false)},
		},
		{
			name:   "MPEG-1 48 kHz",
			inputs: [][]byte{audio([]int{9, 9, 9}, false, 1, false), audio([]int{9, 9}, false, 1, false)},
		},
		{
			name: "mixed sample rates",
			inputs: [][]byte{
				frames(4, 9),
				audio([]int{9, 9, 9}, true, 0, false),
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			titles := make([]string, len(tt.inputs))
			output, index := bind(t, tt.inputs, chaptersFor(titles...))

			header, offset := xingHeader(t, output)
			assert.Zero(t, header.RawBytes[offset+7]&xingFlagTOC,
				"a TOC must not be written for an incompatible stream")

			// Chapter offsets are independent of the TOC and remain valid.
			require.Len(t, index.entries, len(tt.inputs))
			for _, entry := range index.entries {
				assert.False(t, entry.Empty)
				assertFrameSync(t, output, int64(entry.StartOffset))
				assert.NotEmpty(t, parseFramesIn(t, output, int64(entry.StartOffset), int64(entry.EndOffset)))
			}
		})
	}
}

func TestChapterOffsetOverflow(t *testing.T) {
	t.Parallel()

	assert.Equal(t, uint32(0), chapterOffset(0))
	assert.Equal(t, uint32(12345), chapterOffset(12345))
	assert.Equal(t, uint32(id3v2.IgnoredOffset), chapterOffset(-1))
	assert.Equal(t, uint32(id3v2.IgnoredOffset), chapterOffset(int64(id3v2.IgnoredOffset)))
	assert.Equal(t, uint32(id3v2.IgnoredOffset), chapterOffset(int64(id3v2.IgnoredOffset)+1))
	assert.Equal(t, uint32(id3v2.IgnoredOffset), chapterOffset(1<<40))
	assert.Equal(t, uint32(id3v2.IgnoredOffset-1), chapterOffset(int64(id3v2.IgnoredOffset)-1))
}

func normalisedTOCValue(offset, totalBytes int64) byte {
	return byte((offset*xingTOCScale + totalBytes/2) / totalBytes)
}

func TestBuildTOC(t *testing.T) {
	t.Parallel()

	checkpoints := []indexCheckpoint{
		{time: 0, offset: 0},
		{time: time.Second, offset: 1000},
		{time: 2 * time.Second, offset: 3000},
		{time: 4 * time.Second, offset: 4000},
	}

	toc := buildTOC(checkpoints, 4*time.Second, 4000)
	require.Len(t, toc, xingTOCEntries)

	assert.Zero(t, toc[0])
	assert.Equal(t, byte(255), toc[99]) // offset 4000 of 4000 is clamped to 255

	for i := 1; i < xingTOCEntries; i++ {
		assert.LessOrEqual(t, toc[i-1], toc[i])
	}

	// Entry 50 is at 2 seconds, i.e. offset 3000.
	assert.Equal(t, normalisedTOCValue(3000, 4000), toc[50])
	// Entry 25 is at 1 second (offset 1000).
	assert.Equal(t, normalisedTOCValue(1000, 4000), toc[25])
	// Between samples the position is interpolated (1.520s -> ~2020).
	interpolated := offsetAtTime(checkpoints, 4*time.Second*38/xingTOCEntries)
	assert.Equal(t, normalisedTOCValue(interpolated, 4000), toc[38])

	assert.Nil(t, buildTOC(nil, time.Second, 100))
	assert.Nil(t, buildTOC(checkpoints, 0, 100))
	assert.Nil(t, buildTOC(checkpoints, time.Second, 0))
}

func TestBuildTOCClampsAndInterpolates(t *testing.T) {
	t.Parallel()

	// A timeline whose samples are not linear: a large jump after the first
	// sample. Positions between samples are interpolated; both ends clamp.
	checkpoints := []indexCheckpoint{
		{time: 0, offset: 0},
		{time: time.Second, offset: 10},
		{time: 2 * time.Second, offset: 10_000},
	}

	toc := buildTOC(checkpoints, 2*time.Second, 10_000)
	require.Len(t, toc, xingTOCEntries)

	// Before the first sample the position is the first offset.
	assert.Equal(t, byte(0), toc[0])

	// Halfway between the last samples: ~5010 bytes.
	expected := normalisedTOCValue(offsetAtTime(checkpoints, 1500*time.Millisecond), 10_000)
	assert.Equal(t, expected, toc[75])

	// The final entry must never exceed 255 even if rounding asks for more.
	for i := range toc {
		assert.LessOrEqual(t, toc[i], byte(255))
	}

	// An entry that would move backwards after rounding keeps the previous
	// value, so the table stays monotonic even for a decreasing timeline.
	decreasing := buildTOC([]indexCheckpoint{
		{time: 0, offset: 0},
		{time: time.Second, offset: 900},
		{time: 2 * time.Second, offset: 100},
	}, 2*time.Second, 1000)
	require.Len(t, decreasing, xingTOCEntries)
	for i := 1; i < xingTOCEntries; i++ {
		assert.LessOrEqual(t, decreasing[i-1], decreasing[i])
	}

	// A single sample cannot describe a stream; the TOC starts empty and the
	// remaining entries stay at its position.
	single := buildTOC([]indexCheckpoint{{time: 0, offset: 0}}, time.Second, 1000)
	require.Len(t, single, xingTOCEntries)
	for _, b := range single {
		assert.Zero(t, b)
	}

	// Checkpoints beyond the total byte count are clamped.
	beyond := buildTOC([]indexCheckpoint{{time: 0, offset: 0}, {time: time.Second, offset: 5000}}, time.Second, 1000)
	require.Len(t, beyond, xingTOCEntries)
	assert.Equal(t, byte(255), beyond[99])

	// A negative sample cannot happen, but must not produce a negative entry.
	negative := buildTOC([]indexCheckpoint{{time: 0, offset: 0}, {time: time.Second, offset: -50}}, time.Second, 1000)
	require.Len(t, negative, xingTOCEntries)
	assert.Zero(t, negative[99])
}

func TestOffsetAtTime(t *testing.T) {
	t.Parallel()

	checkpoints := []indexCheckpoint{
		{time: 0, offset: 0},
		{time: 2 * time.Second, offset: 100},
	}

	assert.Equal(t, int64(0), offsetAtTime(checkpoints, -time.Second))
	assert.Equal(t, int64(0), offsetAtTime(checkpoints, 0))
	assert.Equal(t, int64(50), offsetAtTime(checkpoints, time.Second))
	assert.Equal(t, int64(100), offsetAtTime(checkpoints, 2*time.Second))
	assert.Equal(t, int64(100), offsetAtTime(checkpoints, 3*time.Second))

	// A regression in the input stream must not produce a decreasing position.
	checkpoints = append(checkpoints, indexCheckpoint{time: 3 * time.Second, offset: 90})
	assert.Equal(t, int64(90), offsetAtTime(checkpoints, 3*time.Second))
}

func TestAddCheckpointRejectsRegressions(t *testing.T) {
	t.Parallel()

	j := &job{indexEnabled: true}

	// beginInput adds the first sample (offset includes the Xing placeholder).
	j.addCheckpoint(0, emptyInfoXingFrameSize)
	j.addCheckpoint(time.Second, emptyInfoXingFrameSize+100)
	require.Len(t, j.checkpoints, 2)
	assert.Equal(t, indexCheckpoint{time: 0, offset: 0}, j.checkpoints[0])
	assert.Equal(t, indexCheckpoint{time: time.Second, offset: 100}, j.checkpoints[1])

	// A regression in time or position is ignored.
	j.addCheckpoint(500*time.Millisecond, emptyInfoXingFrameSize+200)
	j.addCheckpoint(time.Second, emptyInfoXingFrameSize+50)
	j.addCheckpoint(time.Second, emptyInfoXingFrameSize+100)
	require.Len(t, j.checkpoints, 2)

	// A negative offset (only possible for a malformed first call) clamps to 0.
	j.addCheckpoint(2*time.Second, -100)
	require.Len(t, j.checkpoints, 3)
	assert.Equal(t, int64(0), j.checkpoints[2].offset)
}

func TestIndexDisabledByDefault(t *testing.T) {
	t.Parallel()

	// Without the Chapters option nothing is recorded, even though the
	// bind stage calls the index hooks unconditionally.
	j := &job{indexEnabled: false, inputs: make([]io.Reader, 2)}

	j.prepareIndex()
	assert.Nil(t, j.inputStartTimes)

	offset := emptyInfoXingFrameSize
	j.beginInput(0, offset)
	j.addIndexFrame(&mp3lib.MP3Frame{RawBytes: frames(1, 9), SamplingRate: mp3SampleRate44}, offset)
	j.endInput(0)

	assert.Empty(t, j.checkpoints)
	assert.Zero(t, j.totalFrames)
	assert.Zero(t, j.totalBytes)
	assert.Zero(t, j.totalDuration)
	assert.Nil(t, j.toc())
}

func TestChapterTitleWithoutTitle(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "", chapterTitle(id3v2.ChapterFrame{}))
	assert.Equal(t, "x", chapterTitle(id3v2.ChapterFrame{Title: &id3v2.TextFrame{Text: "x"}}))
}

func TestTOCCompatible(t *testing.T) {
	t.Parallel()

	compatible := &job{
		indexEnabled:     true,
		totalFrames:      10,
		referenceRate:    mp3SampleRate44,
		referenceVersion: mp3lib.MPEGVersion1,
		referenceLayer:   mp3lib.MPEGLayerIII,
		totalBytes:       1000,
	}
	assert.True(t, compatible.tocCompatible())

	tests := map[string]func(*job){
		"disabled":        func(j *job) { j.indexEnabled = false },
		"no frames":       func(j *job) { j.totalFrames = 0 },
		"no bytes":        func(j *job) { j.totalBytes = 0 },
		"mixed rates":     func(j *job) { j.mixedRates = true },
		"mp3 at 48k":      func(j *job) { j.referenceRate = 48000 },
		"mpeg2":           func(j *job) { j.referenceVersion = mp3lib.MPEGVersion2 },
		"mpeg1 layer2":    func(j *job) { j.referenceLayer = mp3lib.MPEGLayerII },
		"more than 4 GiB": func(j *job) { j.totalBytes = 1 << 33 },
	}

	for name, mutate := range tests {
		mutate := mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			j := *compatible
			mutate(&j)
			assert.False(t, j.tocCompatible())
		})
	}
}

func TestXingHeaderFrameIsUnchangedWithoutTOC(t *testing.T) {
	t.Parallel()

	// The header layout must stay compatible with the previous implementation:
	// the Xing ID and the frame/byte counters remain at the same positions.
	header := mp3lib.NewXingHeader(12, 3456)

	expected := append([]byte{}, header.RawBytes...)
	writeTOC(header, nil)
	assert.Equal(t, expected, header.RawBytes)

	writeTOC(header, []byte{1, 2, 3})
	assert.Equal(t, expected, header.RawBytes, "an invalid TOC must be ignored")

	// A frame that does not carry a Xing/Info ID is never modified.
	plain := &mp3lib.MP3Frame{RawBytes: frames(1, 9)}
	before := append([]byte{}, plain.RawBytes...)
	writeTOC(plain, make([]byte, xingTOCEntries))
	assert.Equal(t, before, plain.RawBytes)
}

func TestOffsetAtTimeBinarySearch(t *testing.T) {
	t.Parallel()

	// Many samples exercise the binary search and both clamp branches.
	checkpoints := make([]indexCheckpoint, 0, 1000)
	for i := 0; i < 1000; i++ {
		checkpoints = append(checkpoints, indexCheckpoint{
			time:   time.Duration(i) * 100 * time.Millisecond,
			offset: int64(i) * 500,
		})
	}

	// Exact sample, a point between two samples and both ends.
	assert.Equal(t, int64(500*500), offsetAtTime(checkpoints, 500*100*time.Millisecond))
	assert.Equal(t, int64(500*500+250), offsetAtTime(checkpoints, 500*100*time.Millisecond+50*time.Millisecond))
	assert.Equal(t, int64(0), offsetAtTime(checkpoints, 0))
	assert.Equal(t, int64(990*500), offsetAtTime(checkpoints, 99*time.Second))
	assert.Equal(t, int64(999*500), offsetAtTime(checkpoints, time.Hour))

	// A degenerate sample with an identical timestamp must not divide by zero.
	degenerate := []indexCheckpoint{{time: time.Second, offset: 10}, {time: time.Second, offset: 20}}
	assert.Equal(t, int64(20), offsetAtTime(degenerate, 2*time.Second))

	// A decreasing position is clamped so that callers never see a
	// non-monotonic result.
	decreasing := []indexCheckpoint{{time: 0, offset: 0}, {time: time.Second, offset: 100}, {time: 2 * time.Second, offset: 50}}
	// An exact sample is returned as-is (this one is already regressed).
	assert.Equal(t, int64(50), offsetAtTime(decreasing, 2*time.Second))
	// During interpolation the result never drops below the current sample.
	assert.Equal(t, int64(100), offsetAtTime(decreasing, 1500*time.Millisecond))
}
