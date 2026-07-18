package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsAllowedAttachmentType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want bool
	}{
		// Image types.
		{name: "png", path: "photo.png", want: true},
		{name: "jpg", path: "photo.jpg", want: true},
		{name: "jpeg", path: "photo.jpeg", want: true},
		{name: "PNG uppercase", path: "PHOTO.PNG", want: true},

		// Text file types.
		{name: "json", path: "config.json", want: true},
		{name: "yaml", path: "docker-compose.yaml", want: true},
		{name: "yml", path: "docker-compose.yml", want: true},
		{name: "md", path: "README.md", want: true},
		{name: "txt", path: "notes.txt", want: true},
		{name: "css", path: "style.css", want: true},
		{name: "html", path: "index.html", want: true},
		{name: "py", path: "main.py", want: true},
		{name: "go", path: "main.go", want: true},
		{name: "ts", path: "app.ts", want: true},
		{name: "sql", path: "migration.sql", want: true},
		{name: "toml", path: "Cargo.toml", want: true},
		{name: "csv", path: "data.csv", want: true},
		{name: "xml", path: "pom.xml", want: true},
		{name: "env", path: ".env", want: true},
		{name: "conf", path: "nginx.conf", want: true},
		{name: "tf", path: "main.tf", want: true},
		{name: "log", path: "app.log", want: true},

		// Case insensitivity.
		{name: "JSON uppercase", path: "CONFIG.JSON", want: true},
		{name: "MD mixed case", path: "ReadMe.Md", want: true},

		// Full paths.
		{name: "full path", path: "/home/user/project/config.json", want: true},
		{name: "relative path", path: "../src/main.go", want: true},

		// Unsupported types.
		{name: "pdf", path: "doc.pdf", want: false},
		{name: "zip", path: "archive.zip", want: false},
		{name: "docx", path: "report.docx", want: false},
		{name: "exe", path: "binary.exe", want: false},
		// Makefile/Dockerfile are extensionless but allowlisted by name —
		// the ".makefile"/".dockerfile" extension entries never matched them.
		{name: "no extension allowlisted", path: "Makefile", want: true},
		{name: "no extension not allowlisted", path: "LICENSE", want: false},
		{name: "mp4", path: "video.mp4", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, IsAllowedAttachmentType(tt.path))
		})
	}
}

func TestIsAllowedImageType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "png", path: "photo.png", want: true},
		{name: "jpg", path: "photo.jpg", want: true},
		{name: "jpeg", path: "photo.jpeg", want: true},
		{name: "PNG uppercase", path: "PHOTO.PNG", want: true},
		{name: "json", path: "config.json", want: false},
		{name: "pdf", path: "doc.pdf", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, IsAllowedImageType(tt.path))
		})
	}
}

func TestAllAllowedAttachmentTypes(t *testing.T) {
	t.Parallel()

	types := AllAllowedAttachmentTypes()

	// Should include all image types.
	for _, ext := range AllowedImageTypes {
		require.Contains(t, types, ext)
	}

	// Should include all text file types.
	for _, ext := range AllowedTextFileTypes {
		require.Contains(t, types, ext)
	}

	// Should have combined length.
	require.Len(t, types, len(AllowedImageTypes)+len(AllowedTextFileTypes))
}

// TestIsAllowedAttachmentType_ExtensionlessNames verifies Dockerfile/Makefile
// are attachable: suffix matching can't cover them (no dot), so the old
// ".dockerfile"/".makefile" entries never matched the real files.
func TestIsAllowedAttachmentType_ExtensionlessNames(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"Dockerfile", "Makefile", "GNUmakefile", "/repo/sub/Dockerfile", "some/Makefile"} {
		require.True(t, IsAllowedAttachmentType(path), path)
	}
	// The dotted variants still work for files that genuinely have them.
	require.True(t, IsAllowedAttachmentType("build.dockerfile"))
	// Non-allowlisted extensionless names stay rejected.
	require.False(t, IsAllowedAttachmentType("LICENSE"))
}

// TestSniffAttachmentMIME verifies the binary-content guard: a file allowed
// by a text extension whose content sniffs as binary (e.g. a NUL byte in the
// first 512 bytes) must be reported not-ok instead of becoming a bogus
// binary file part or byte soup inlined into the prompt.
func TestSniffAttachmentMIME(t *testing.T) {
	t.Parallel()

	mt, ok := SniffAttachmentMIME([]byte("plain text content"))
	require.True(t, ok)
	require.Contains(t, mt, "text/plain")

	// PNG magic bytes → image, ok.
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	mt, ok = SniffAttachmentMIME(png)
	require.True(t, ok)
	require.Equal(t, "image/png", mt)

	// NUL byte in the sniff window → application/octet-stream, not ok.
	mt, ok = SniffAttachmentMIME([]byte("looks like a log\x00but binary"))
	require.False(t, ok, "binary-sniffing content must be rejected")
	require.Equal(t, "application/octet-stream", mt)

	// Empty content sniffs as text; harmless.
	_, ok = SniffAttachmentMIME(nil)
	require.True(t, ok)
}
