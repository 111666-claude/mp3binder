package mp3binder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/crra/id3v2/v2"
	"github.com/dmulholl/mp3lib"
)

var ErrUnusableOption = errors.New("unusable option")

const (
	emptyInfoXingFrameSize int64 = 209
	tagTitle                     = "TIT2"
)

type job struct {
	context   context.Context
	output    io.WriteSeeker
	audioOnly io.ReadWriteSeeker
	inputs    []io.Reader

	tagResolver tagResolver
	tag         *id3v2.Tag
	metadata    []*id3v2.Tag

	inputDurations  []time.Duration
	stageVisitor    stageVisitor
	metadataVisitor metadataVisitor
	bindVisitor     bindVisitor
	tagCopyVisitor  tagCopyVisitor
	tagApplyVisitor tagApplyVisitor

	// chapter index state
	indexEnabled     bool
	indexVisitor     chapterIndexVisitor
	chapterResolver  func(index int, chapterIndex int) (bool, string)
	chapters         []id3v2.ChapterFrame
	chapterInputs    []int
	chapterIDs       []string
	inputStartTimes  []time.Duration
	inputStartBytes  []int64
	inputByteCounts  []int64
	inputFrameCount  []uint32
	currentInput     int
	referenceRate    int
	referenceVersion byte
	referenceLayer   byte
	mixedRates       bool
	totalDuration    time.Duration
	totalBytes       int64
	totalFrames      uint64
	checkpoints      []indexCheckpoint
}

// indexCheckpoint is a sample of the output timeline: a byte offset and the
// playback time at which the audio stored there starts.
type indexCheckpoint struct {
	time   time.Duration
	offset int64
}

type namedJobProcessor struct {
	name      string
	processor jobProcessor
}

type (
	stageVisitor    func(string, string)
	metadataVisitor func(index int, tags map[string]string)
	bindVisitor     func(int)
	tagCopyVisitor  func(string, string, error)
	tagApplyVisitor func(string, string, error)
	// chapterIndexVisitor receives one entry per chapter with its position in
	// the output file (see ChapterIndexEntry).
	chapterIndexVisitor func(ChapterIndexEntry)
)

type tagResolver interface {
	DescriptionFor(string) (string, error)
}

type binder struct {
	tagResolver tagResolver
}

func New(tagResolver tagResolver) *binder {
	return &binder{
		tagResolver: tagResolver,
	}
}

func (b *binder) Bind(parent context.Context, output io.WriteSeeker, audioOnly io.ReadWriteSeeker, input []io.Reader, o ...any) error {
	options := make([]Option, len(o))

	for i, op := range o {
		option, ok := op.(Option)
		if !ok {
			return ErrUnusableOption
		}

		options[i] = option
	}

	return Bind(parent, b.tagResolver, output, audioOnly, input, options...)
}

// Bind joins the inputs into the output file. The audio is first collected in
// audioOnly (which is rewound and copied to output after the ID3v2 tag is
// written), so that the size of the final tag is known before the chapter
// index is computed. Offsets of the index are relative to the beginning of
// output, including the ID3v2 tag.
func Bind(parent context.Context, tagResolver tagResolver, output io.WriteSeeker, audioOnly io.ReadWriteSeeker, input []io.Reader, options ...Option) error {
	j := &job{
		context:   parent,
		output:    output,
		audioOnly: audioOnly,
		inputs:    input,

		tagResolver: tagResolver,

		tag:            id3v2.NewEmptyTag(),
		inputDurations: make([]time.Duration, len(input)),
		metadata:       make([]*id3v2.Tag, len(input)),

		stageVisitor:    func(string, string) {},
		metadataVisitor: func(int, map[string]string) {},
		bindVisitor:     func(int) {},
		tagApplyVisitor: func(string, string, error) {},
		tagCopyVisitor:  func(string, string, error) {},
		indexVisitor:    func(ChapterIndexEntry) {},
	}

	jobProcessors := make(map[stage][]namedJobProcessor)

	options = append(options, bindAudioOnly, notifyMetadata, buildChapters, writeChapterIndex, writeMetadata, combineMetadataAndAudio)

	for _, o := range options {
		stage, name, processor := o()
		jobProcessors[stage] = append(jobProcessors[stage], namedJobProcessor{
			name:      name,
			processor: processor,
		})
	}

	// process all stages
	for s := stage(0); s < stageLastElement; s++ {
		for _, p := range jobProcessors[s] {
			if s != stageInit {
				j.stageVisitor(s.String(), p.name)
			}
			if err := p.processor(j); err != nil {
				return err
			}
		}
	}

	return nil
}

func writeMetadata() (stage, string, jobProcessor) {
	return stageWriteMetadata, "writing metadata", func(j *job) error {
		if _, err := j.tag.WriteTo(j.output); err != nil {
			return err
		}

		return nil
	}
}

// bindStats collects the counters that describe the bound audio stream.
type bindStats struct {
	frames           uint32
	bytes            uint32
	firstBitrate     int
	multipleBitrates bool
	offset           int64
}

// add accounts for a frame that is written next. offset is the position of the
// frame in the audio-only file, including the Xing/Info placeholder.
func (s *bindStats) add(frame *mp3lib.MP3Frame) {
	if s.firstBitrate == 0 {
		s.firstBitrate = frame.BitRate
	} else if !s.multipleBitrates && s.firstBitrate != frame.BitRate {
		s.multipleBitrates = true
	}

	s.frames++
	s.bytes += uint32(len(frame.RawBytes))
	s.offset += int64(len(frame.RawBytes))
}

func bindAudioOnly() (stage, string, jobProcessor) {
	return stageBind, "Binding", func(j *job) error {
		if _, err := j.audioOnly.Write(make([]byte, emptyInfoXingFrameSize)); err != nil {
			return err
		}

		j.prepareIndex()

		stats := &bindStats{offset: emptyInfoXingFrameSize}

		for fileIndex, reader := range j.inputs {
			if err := j.bindInput(fileIndex, reader, stats); err != nil {
				return err
			}
		}

		if err := writeBitrateHeader(j.audioOnly, stats, j.indexEnabled, j.toc()); err != nil {
			return err
		}

		return nil
	}
}

// bindInput appends all audio frames of one input to the audio-only file.
func (j *job) bindInput(fileIndex int, reader io.Reader, stats *bindStats) error {
	j.bindVisitor(fileIndex)

	if j.metadata[fileIndex] == nil {
		j.metadata[fileIndex] = id3v2.NewEmptyTag()
	}

	j.beginInput(fileIndex, stats.offset)
	defer j.endInput(fileIndex)

	for i := 0; ; i++ {
		select {
		case <-j.context.Done():
			return j.context.Err()
		default:
			obj := mp3lib.NextObject(reader)
			if obj == nil {
				return nil
			}

			switch obj := obj.(type) {
			case *mp3lib.MP3Frame:
				// A leading Xing/VBRI frame is a header of the input, not
				// audio; it must neither be bound nor indexed.
				if i == 0 && (mp3lib.IsXingHeader(obj) || mp3lib.IsVbriHeader(obj)) {
					continue
				}

				if j.indexEnabled {
					j.addIndexFrame(obj, stats.offset)
				}

				if _, err := j.audioOnly.Write(obj.RawBytes); err != nil {
					return err
				}

				j.inputDurations[fileIndex] += duration(obj)
				stats.add(obj)

			case *mp3lib.ID3v2Tag:
				tag, err := id3v2.ParseReader(bytes.NewReader(obj.RawBytes), id3v2.Options{Parse: true})
				if err != nil {
					return err
				}

				for id := range tag.AllFrames() {
					j.metadata[fileIndex].AddFrame(id, tag.GetLastFrame(id))
				}

			default:
				continue
			}
		}
	}
}

func tagToMap(tag *id3v2.Tag) map[string]string {
	m := make(map[string]string)
	if tag == nil || !tag.HasFrames() {
		return m
	}
	for id := range tag.AllFrames() {
		f := tag.GetLastFrame(id)
		if tf, ok := f.(id3v2.TextFrame); ok {
			m[id] = tf.Text
		}
	}

	return m
}

func notifyMetadata() (stage, string, jobProcessor) {
	return stageApplyMetadata, "notify visitor", func(j *job) error {
		for i, t := range j.metadata {
			j.metadataVisitor(i, tagToMap(t))
		}

		return nil
	}
}

func writeBitrateHeader(out io.WriteSeeker, stats *bindStats, withIndex bool, toc []byte) error {
	var emptyInfoXingFrameOffset int64 = int64(stats.bytes) + emptyInfoXingFrameSize
	if _, err := out.Seek(emptyInfoXingFrameOffset*-1, io.SeekCurrent); err != nil {
		return fmt.Errorf("can not seek to info/xing frame, %v", err)
	}

	header := mp3lib.NewXingHeader(stats.frames, stats.bytes)

	// The Xing marker denotes a VBR stream and the Info marker a CBR stream.
	// The historical output of this tool used the opposite labels, which is
	// kept for the original (index-less) merge mode so that its output does
	// not change. With an index the standard labels are used: a VBR file is
	// then recognised as VBR by players, which is a precondition for them to
	// consult the TOC without being asked to.
	marker := "Xing"
	if stats.multipleBitrates != withIndex {
		marker = "Info"
	}

	offset := 4 + getSideInfoSize(header)
	copy(header.RawBytes[offset:offset+4], marker)

	if withIndex {
		writeTOC(header, toc)
	}

	if _, err := out.Write(header.RawBytes); err != nil {
		return fmt.Errorf("can not write xing/info header, %v", err)
	}

	if _, err := out.Seek(0, io.SeekCurrent); err != nil {
		return fmt.Errorf("can not seek to end of file, %v", err)
	}

	return nil
}

func duration(frame *mp3lib.MP3Frame) time.Duration {
	return time.Duration(int64(float64(time.Millisecond) * (1000 / float64(frame.SamplingRate)) * float64(frame.SampleCount)))
}

func getSideInfoSize(frame *mp3lib.MP3Frame) int {
	var size int

	if frame.MPEGLayer == mp3lib.MPEGLayerIII {
		if frame.MPEGVersion == mp3lib.MPEGVersion1 {
			if frame.ChannelMode == mp3lib.Mono {
				size = 17
			} else {
				size = 32
			}
		} else {
			if frame.ChannelMode == mp3lib.Mono {
				size = 9
			} else {
				size = 17
			}
		}
	}

	return size
}

func combineMetadataAndAudio() (stage, string, jobProcessor) {
	return stageCombineId3AndAudio, "combining metadata and audio", func(j *job) error {
		if _, err := j.audioOnly.Seek(0, io.SeekStart); err != nil {
			return err
		}

		if _, err := io.Copy(j.output, j.audioOnly); err != nil {
			return err
		}

		return nil
	}
}
