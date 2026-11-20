package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carolynvs/aferox"
	"github.com/crra/mp3binder/mp3binder"
	"github.com/stretchr/testify/assert"
)

type testCollector struct {
	err error

	parent    context.Context
	output    io.WriteSeeker
	audioOnly io.ReadWriteSeeker
	input     []io.Reader
	options   []mp3binder.Option
}

func (t *testCollector) Bind(parent context.Context, output io.WriteSeeker, audioOnly io.ReadWriteSeeker, input []io.Reader, options ...any) error {
	t.parent = parent
	t.output = output
	t.audioOnly = audioOnly
	t.input = input
	// t.options = options

	return t.err
}

func TestCreateEmptyFile(t *testing.T) {
	t.Parallel()
	tc := &testCollector{}
	root, fs := newTestFilesystem()

	a := newDefaultApplication(aferox.NewAferox(root, fs))
	a.binder = tc
	a.outputPath = filepath.Join(root, validOutputFile)

	err := a.run(nil, nil)
	if assert.NoError(t, err) {
		_, err := fs.Stat(a.outputPath)
		assert.NoError(t, err)
	}
}

func TestRemoveOutputFileOnError(t *testing.T) {
	t.Parallel()
	tc := &testCollector{err: assert.AnError}
	root, fs := newTestFilesystem()

	a := newDefaultApplication(aferox.NewAferox(root, fs))
	a.binder = tc
	a.outputPath = filepath.Join(root, validOutputFile)

	err := a.run(nil, nil)
	if assert.Error(t, err) {
		_, err := fs.Stat(a.outputPath)
		assert.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestMediaFiles(t *testing.T) {
	t.Parallel()
	tc := &testCollector{}
	root, fs := newTestFilesystem()
	mediaFiles := withTwoValidFiles(fs, root)

	a := newDefaultApplication(aferox.NewAferox(root, fs))
	a.binder = tc
	a.mediaFiles = mediaFiles
	a.outputPath = filepath.Join(root, validOutputFile)

	err := a.run(nil, nil)
	if assert.NoError(t, err) {
		assert.Equal(t, len(mediaFiles), len(tc.input))
	}
}

func TestInterlaceWithTwo(t *testing.T) {
	t.Parallel()
	tc := &testCollector{}
	root, fs := newTestFilesystem()
	mediaFiles := withTwoValidFiles(fs, root)
	_ = makeEmptyFiles(fs, root, validInterlaceFile1, validInterlaceFile2)

	a := newDefaultApplication(aferox.NewAferox(root, fs))

	a.binder = tc
	a.mediaFiles = mediaFiles
	a.interlaceFile = filepath.Join(root, validInterlaceFile1)
	a.outputPath = filepath.Join(root, validOutputFile)

	err := a.run(nil, nil)
	if assert.NoError(t, err) {
		assert.Equal(t, filepathJoin(root, validFileName1, validInterlaceFile1, validFileName2), a.mediaFiles)
		assert.Equal(t, len(a.mediaFiles), len(tc.input))
	}
}

func TestInterlaceWithThree(t *testing.T) {
	t.Parallel()
	tc := &testCollector{}
	root, fs := newTestFilesystem()
	mediaFiles := withThreeValidFiles(fs, root)
	_ = makeEmptyFiles(fs, root, validInterlaceFile1, validInterlaceFile2)

	a := newDefaultApplication(aferox.NewAferox(root, fs))
	a.binder = tc
	a.mediaFiles = mediaFiles
	a.interlaceFile = filepath.Join(root, validInterlaceFile1)
	a.outputPath = filepath.Join(root, validOutputFile)

	err := a.run(nil, nil)
	if assert.NoError(t, err) {
		assert.Equal(t, filepathJoin(root, validFileName1, validInterlaceFile1, validFileName2, validInterlaceFile1, validFileName3), a.mediaFiles)
		assert.Equal(t, len(a.mediaFiles), len(tc.input))
	}
}

func TestChapterIndexUsesVerboseStatusPrinter(t *testing.T) {
	t.Parallel()

	builder := &strings.Builder{}
	printer := newVerbosePrinter(builder)
	observer := printer.newChapterIndexObserver()

	observer(mp3binder.ChapterIndexEntry{Chapter: 1, StartOffset: 1000, Frames: 10, TOC: true})
	observer(mp3binder.ChapterIndexEntry{Chapter: 2, StartOffset: 2000, Frames: 10, TOC: true})
	observer(mp3binder.ChapterIndexEntry{Chapter: 3, Empty: true})
	observer(mp3binder.ChapterIndexEntry{Chapter: 4, OffsetOverflow: true})

	output := builder.String()
	assert.Contains(t, output, "Xing table of contents")
	assert.Contains(t, output, "Chapter 1 at byte offset 1000 (frame 10)")
	assert.Contains(t, output, "Chapter 2 at byte offset 2000 (frame 10)")
	assert.Contains(t, output, "Chapter 3 has no audio")
	assert.Contains(t, output, "Chapter 4 starts beyond the 32-bit offset")
	assert.Equal(t, 1, strings.Count(output, "Xing table of contents"))
}
