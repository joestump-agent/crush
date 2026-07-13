package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFocusCycleNonCompact(t *testing.T) {
	t.Parallel()

	focus := uiFocusEditor
	focus = nextFocus(focus, false)
	require.Equal(t, uiFocusMain, focus, "editor → main")
	focus = nextFocus(focus, false)
	require.Equal(t, uiFocusSidebar, focus, "main → sidebar")
	focus = nextFocus(focus, false)
	require.Equal(t, uiFocusEditor, focus, "sidebar → editor")
}

func TestFocusCycleCompact(t *testing.T) {
	t.Parallel()

	focus := uiFocusEditor
	focus = nextFocus(focus, true)
	require.Equal(t, uiFocusMain, focus, "editor → main (compact)")
	focus = nextFocus(focus, true)
	require.Equal(t, uiFocusEditor, focus, "main → editor (compact)")
}

func nextFocus(current uiFocusState, isCompact bool) uiFocusState {
	if isCompact {
		switch current {
		case uiFocusEditor:
			return uiFocusMain
		default:
			return uiFocusEditor
		}
	}
	switch current {
	case uiFocusEditor:
		return uiFocusMain
	case uiFocusMain:
		return uiFocusSidebar
	default:
		return uiFocusEditor
	}
}
