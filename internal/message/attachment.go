package message

import (
	"slices"
	"strings"
)

type Attachment struct {
	FilePath string
	FileName string
	MimeType string
	Content  []byte
}

// textMimePrefixes are MIME type prefixes that should be treated as text.
var textMimePrefixes = []string{
	"text/",
	"application/json",
	"application/xml",
	"application/yaml",
	"application/x-yaml",
	"application/javascript",
	"application/typescript",
	"application/x-sh",
	"application/x-shellscript",
	"application/toml",
	"application/x-toml",
	"application/sql",
	"application/x-sql",
	"application/graphql",
	"application/x-graphql",
}

// IsTextMIME reports whether a MIME type should be treated as text and
// inlined into the prompt. This includes standard text/* MIME types as well
// as common application/* types that are fundamentally text (JSON, YAML,
// XML, etc.). Every place that decides "inline as text vs. send as a binary
// file part" MUST use this single predicate: when the initial send and the
// history rebuild (ToAIMessage) disagree, a text attachment that worked on
// turn one comes back on turn two as a bogus binary file part.
func IsTextMIME(mimeType string) bool {
	for _, prefix := range textMimePrefixes {
		if strings.HasPrefix(mimeType, prefix) {
			return true
		}
	}
	return false
}

// IsText reports whether the attachment should be treated as text and
// inlined into the prompt. See IsTextMIME.
func (a Attachment) IsText() bool {
	return IsTextMIME(a.MimeType)
}

func (a Attachment) IsImage() bool    { return strings.HasPrefix(a.MimeType, "image/") }
func (a Attachment) IsMarkdown() bool { return a.MimeType == "text/markdown" }

// ContainsTextAttachment returns true if any of the attachments is a text attachment.
func ContainsTextAttachment(attachments []Attachment) bool {
	return slices.ContainsFunc(attachments, func(a Attachment) bool {
		return a.IsText()
	})
}
