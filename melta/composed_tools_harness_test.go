package melta_test

// The external suite's shared tool fixtures: the scripted model streams, the
// one-call preparation helper, the background-job identity reader, and the
// focused task Harness fixture the composed permission row drives.

import (
	"io"
	"regexp"

	"github.com/Syntheresis/melta/model"
)

const (
	testSessionID = "0123456789abcdef0123456789abcdef"
	testEntryID   = "fedcba9876543210fedcba9876543210"
)

// backgroundIDPattern extracts one started background job identity from a
// retained immediate result.
var backgroundIDPattern = regexp.MustCompile("Command running in the background with ID: `([0-9a-f]{8})`")

// backgroundJobID extracts the last started background job identity from one
// model request's accumulated text.
func backgroundJobID(text string) string {
	matches := backgroundIDPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

type deltaStream struct {
	deltas []model.StreamDelta
	i      int
}

func (s *deltaStream) Recv() (model.StreamDelta, error) {
	if s.i >= len(s.deltas) {
		return model.StreamDelta{}, io.EOF
	}
	d := s.deltas[s.i]
	s.i++
	return d, nil
}

func (s *deltaStream) Close() error { return nil }

func streamOf(deltas ...model.StreamDelta) model.Stream {
	return &deltaStream{deltas: deltas}
}

// toolCallStream is one completed assistant turn publishing one tool call.
func toolCallStream(id, name, args string) model.Stream {
	return streamOf(
		model.StreamDelta{HasChoice: true, ToolFragments: []model.ToolCallFragment{{ID: id, Name: name, ArgumentFragment: args}}},
		model.StreamDelta{HasChoice: true, FinishReason: "tool_calls"},
	)
}

// stopStream is one completed assistant turn with plain text and no calls.
func stopStream(text string) model.Stream {
	return streamOf(
		model.StreamDelta{HasChoice: true, ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: text}}},
		model.StreamDelta{HasChoice: true, FinishReason: "stop"},
	)
}
