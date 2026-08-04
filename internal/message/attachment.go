package message

import (
	"slices"
	"strings"
)

// AttachmentKind names what an attachment came from, for presentation.
//
// It exists because MIME type cannot answer the question: an MCP prompt's
// content is text that must reach the model intact, so it necessarily shares
// a MIME type with skills and other text attachments while needing its own
// icon and label rule. Sniffing harder only makes the coupling more brittle.
//
// The zero value is [AttachmentKindFile], so attachments built before this
// field existed — and every call site that does not care — keep the previous
// behavior, which derives presentation from the MIME type.
type AttachmentKind string

const (
	// AttachmentKindFile is a file or MCP resource pulled in by an @mention,
	// or anything else that is fundamentally file-shaped. Presentation falls
	// back to MIME sniffing for these.
	AttachmentKindFile AttachmentKind = ""
	// AttachmentKindMCPPrompt is a resolved MCP prompt, attached by the
	// inline "/server:prompt" completion.
	AttachmentKindMCPPrompt AttachmentKind = "mcp_prompt"
)

type Attachment struct {
	FilePath string
	FileName string
	MimeType string
	Content  []byte
	// Kind distinguishes attachments whose presentation cannot be derived
	// from MimeType. See [AttachmentKind].
	Kind AttachmentKind
	// PromptArgCount is the number of arguments a resolved MCP prompt was
	// invoked with, shown on its chip. Only meaningful for
	// [AttachmentKindMCPPrompt].
	PromptArgCount int
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
