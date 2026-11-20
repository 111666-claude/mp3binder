//go:generate stringer -type=stage -trimprefix stage
package mp3binder

import (
	"fmt"
	"io"
	"time"

	"github.com/crra/id3v2/v2"
)

// stage defines an action sequence.
type stage int

const (
	stageInit stage = iota

	stageBind

	stageCopyMetadata
	stageApplyMetadata
	stageBuildChapers
	stageWriteIndex
	stageWriteMetadata

	stageCombineId3AndAudio

	// https://stackoverflow.com/questions/64178176/how-to-create-an-enum-and-iterate-over-it
	stageLastElement
)

type (
	// jobProcessor is a function that is called for the current job.
	jobProcessor func(*job) error
	// Option is a function that offers functional configuration
	// see: https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html
	Option func() (stage, string, jobProcessor)
)

// ActionVisitor registers a callback to receive the name of the current action
// to be executed.
func ActionVisitor(f stageVisitor) Option {
	return func() (stage, string, jobProcessor) {
		return stageInit, "stage visitor", func(j *job) error {
			j.stageVisitor = f

			return nil
		}
	}
}

// BindVisitor registers a callback to receive the index of the current
// file to be bound.
func BindVisitor(f bindVisitor) Option {
	return func() (stage, string, jobProcessor) {
		return stageInit, "bind visitor", func(j *job) error {
			j.bindVisitor = f

			return nil
		}
	}
}

// TagApplyVisitor registers a callback the receive a key/value pair that is
// currently applied to the output file.
func TagApplyVisitor(f tagApplyVisitor) Option {
	return func() (stage, string, jobProcessor) {
		return stageInit, "tag visitor", func(j *job) error {
			j.tagApplyVisitor = f

			return nil
		}
	}
}

// TagCopyVisitor registers a callback to receive a key/value pair that is
// currently copied from an input file.
func TagCopyVisitor(f tagCopyVisitor) Option {
	return func() (stage, string, jobProcessor) {
		return stageInit, "tag copy visitor", func(j *job) error {
			j.tagCopyVisitor = f

			return nil
		}
	}
}

// MetadataVisitor registers a callback to receive the parsed metadata of the
// media files.
func MetadataVisitor(f metadataVisitor) Option {
	return func() (stage, string, jobProcessor) {
		return stageInit, "metadata visitor", func(j *job) error {
			j.metadataVisitor = f

			return nil
		}
	}
}

// CopyMetadataFrom copies the metadata from an input file to the output file (incl. cover files).
func CopyMetadataFrom(index int, errNoTagsInTemplate error) Option {
	return func() (stage, string, jobProcessor) {
		return stageCopyMetadata, "copy metadata", func(j *job) error {
			template, err := id3v2.ParseReader(j.inputs[index], id3v2.Options{Parse: true})
			if err != nil {
				return err
			}

			if !template.HasFrames() {
				j.tagCopyVisitor("", "", errNoTagsInTemplate)
				return nil
			}

			for id := range template.AllFrames() {
				f := template.GetLastFrame(id)
				switch ff := f.(type) {
				case id3v2.TextFrame:
					j.tagCopyVisitor(id, ff.Text, nil)
				case id3v2.PictureFrame:
					j.tagCopyVisitor(id, fmt.Sprintf("Image of type '%s'", ff.MimeType), nil)
				case id3v2.ChapterFrame:
					continue
				}

				j.tag.AddFrame(id, f)
			}

			return nil
		}
	}
}

// ApplyTextMetadata applies key/value pairs of text as metadata to the bounded file.
func ApplyTextMetadata(f func(map[string]string) map[string]string) Option {
	return func() (stage, string, jobProcessor) {
		return stageApplyMetadata, "applying text metadata", func(j *job) error {
			for id, value := range f(tagToMap(j.tag)) {
				description, err := j.tagResolver.DescriptionFor(id)
				if err != nil {
					j.tagApplyVisitor(id, "", fmt.Errorf("tag '%s': %w", id, err))
				} else {
					j.tagApplyVisitor(fmt.Sprintf("%s (%s)", description, id), value, nil)
				}

				if value == "" {
					j.tag.DeleteFrames(id)
					continue
				}

				j.tag.AddFrame(id, &id3v2.TextFrame{Encoding: j.tag.DefaultEncoding(), Text: value})
			}

			return nil
		}
	}
}

const coverType = "Front cover"

// Cover assigns a file as cover to the bounded file.
func Cover(mimeType string, r io.Reader) Option {
	return func() (stage, string, jobProcessor) {
		return stageApplyMetadata, "adding cover", func(j *job) error {
			frontCoverPicture, err := io.ReadAll(r)
			if err != nil {
				return err
			}

			j.tagApplyVisitor(coverType, mimeType, nil)

			j.tag.AddAttachedPicture(id3v2.PictureFrame{
				Encoding:    j.tag.DefaultEncoding(),
				MimeType:    mimeType,
				PictureType: id3v2.PTFrontCover,
				Description: coverType,
				Picture:     frontCoverPicture,
			})

			return nil
		}
	}
}

// Chapters uses a callback function to resolve the title of the chapter for a file that bound.
//
// When the callback skips a file, the file's audio is still appended to the
// output but no chapter is created for it (e.g. an interlace file). Enabling
// chapters also enables the chapter index: every chapter start is recorded as
// a byte offset in the bound file and a Xing/Info TOC is written for coarse
// seeking.
func Chapters(resolveFunc func(index int, chapterIndex int) (bool, string)) Option {
	return func() (stage, string, jobProcessor) {
		return stageInit, "chapter resolver", func(j *job) error {
			j.chapterResolver = resolveFunc
			j.indexEnabled = true

			return nil
		}
	}
}

// buildChapters creates one chapter frame per input file (as accepted by the
// resolver registered through Chapters). The byte offsets are filled in later
// by the index stage, once the size of the final tag is known.
func buildChapters() (stage, string, jobProcessor) {
	return stageBuildChapers, "adding chapters", func(j *job) error {
		if j.chapterResolver == nil {
			return nil
		}

		var start time.Duration

		chapterIndex := 1
		for i, numberOfFiles := 0, len(j.inputDurations); i < numberOfFiles; i++ {
			// The timeline follows the bound audio, including inputs that
			// are skipped as chapters (e.g. interlace files). The recorded
			// start time is exact even for inputs with an unusual frame
			// length because it comes from the bind stage itself.
			start = j.inputStartTimes[i]

			end := start + j.inputDurations[i]

			createChapter, chapterTitle := j.chapterResolver(i, chapterIndex)
			if !createChapter {
				continue
			}

			// The element ID is derived from the position in the output, not
			// from the title: duplicate titles are valid and must not produce
			// duplicate (and therefore ignored) chapter frames.
			chapter := id3v2.ChapterFrame{
				ElementID:   fmt.Sprintf("c%d", chapterIndex),
				StartTime:   start,
				EndTime:     end,
				StartOffset: id3v2.IgnoredOffset,
				EndOffset:   id3v2.IgnoredOffset,
				Title: &id3v2.TextFrame{
					Encoding: id3v2.EncodingUTF8,
					Text:     chapterTitle,
				},
			}

			j.tag.AddChapterFrame(chapter)
			j.chapters = append(j.chapters, chapter)
			j.chapterInputs = append(j.chapterInputs, i)

			j.tagApplyVisitor(fmt.Sprintf("Chapter: %d from '%s' to '%s'", chapterIndex, start.Round(time.Second), end.Round(time.Second)), chapterTitle, nil)

			j.chapterIDs = append(j.chapterIDs, chapter.ElementID)
			chapterIndex++
		}

		if len(j.chapterIDs) > 0 {
			j.tag.AddChapterTocFrame(id3v2.ChapterTocFrame{
				ElementID:  "MainChapterToc",
				TopLevel:   true,
				Ordered:    true,
				ChapterIds: j.chapterIDs,
			})
		}

		return nil
	}
}

// ChapterIndexVisitor registers a callback that receives each chapter with its
// position in the final output file. The callback is invoked after all audio is
// bound and the final tag size is known.
func ChapterIndexVisitor(f chapterIndexVisitor) Option {
	return func() (stage, string, jobProcessor) {
		return stageInit, "chapter index visitor", func(j *job) error {
			j.indexVisitor = f

			return nil
		}
	}
}
