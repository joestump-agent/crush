package model

import (
	"context"

	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
)

// Stubs for test workspace fakes that don't exercise Sidekick features.
// These satisfy the workspace.Workspace interface's Sidekick methods by
// reporting everything unavailable/empty so the non-Sidekick tests run
// without triggering Sidekick code paths.

func (w *a2uiActionWorkspace) SidekickAvailable() bool                   { return false }
func (w *a2uiActionWorkspace) SidekickRun(context.Context, string) error { return nil }
func (w *a2uiActionWorkspace) SidekickCancel()                           {}
func (w *a2uiActionWorkspace) SidekickIsBusy() bool                      { return false }
func (w *a2uiActionWorkspace) SidekickClear(context.Context) error       { return nil }
func (w *a2uiActionWorkspace) SidekickSubscribe(context.Context) <-chan pubsub.Event[message.Message] {
	return nil
}

func (w *a2uiActionWorkspace) SidekickDashboardSubscribe(context.Context) <-chan pubsub.Event[agenttools.SidekickSurface] {
	return nil
}

func (w *a2uiActionWorkspace) SidekickModel() config.SelectedModel         { return config.SelectedModel{} }
func (w *a2uiActionWorkspace) SidekickSetModel(config.SelectedModel) error { return nil }

func (w attachmentClickWorkspace) SidekickAvailable() bool                   { return false }
func (w attachmentClickWorkspace) SidekickRun(context.Context, string) error { return nil }
func (w attachmentClickWorkspace) SidekickCancel()                           {}
func (w attachmentClickWorkspace) SidekickIsBusy() bool                      { return false }
func (w attachmentClickWorkspace) SidekickClear(context.Context) error       { return nil }
func (w attachmentClickWorkspace) SidekickSubscribe(context.Context) <-chan pubsub.Event[message.Message] {
	return nil
}

func (w attachmentClickWorkspace) SidekickDashboardSubscribe(context.Context) <-chan pubsub.Event[agenttools.SidekickSurface] {
	return nil
}
func (w attachmentClickWorkspace) SidekickModel() config.SelectedModel         { return config.SelectedModel{} }
func (w attachmentClickWorkspace) SidekickSetModel(config.SelectedModel) error { return nil }

func (w *channelWorkspace) SidekickAvailable() bool                   { return false }
func (w *channelWorkspace) SidekickRun(context.Context, string) error { return nil }
func (w *channelWorkspace) SidekickCancel()                           {}
func (w *channelWorkspace) SidekickIsBusy() bool                      { return false }
func (w *channelWorkspace) SidekickClear(context.Context) error       { return nil }
func (w *channelWorkspace) SidekickSubscribe(context.Context) <-chan pubsub.Event[message.Message] {
	return nil
}

func (w *channelWorkspace) SidekickDashboardSubscribe(context.Context) <-chan pubsub.Event[agenttools.SidekickSurface] {
	return nil
}
func (w *channelWorkspace) SidekickModel() config.SelectedModel         { return config.SelectedModel{} }
func (w *channelWorkspace) SidekickSetModel(config.SelectedModel) error { return nil }

func (w *slashCommandWorkspace) SidekickAvailable() bool                   { return false }
func (w *slashCommandWorkspace) SidekickRun(context.Context, string) error { return nil }
func (w *slashCommandWorkspace) SidekickCancel()                           {}
func (w *slashCommandWorkspace) SidekickIsBusy() bool                      { return false }
func (w *slashCommandWorkspace) SidekickClear(context.Context) error       { return nil }
func (w *slashCommandWorkspace) SidekickSubscribe(context.Context) <-chan pubsub.Event[message.Message] {
	return nil
}

func (w *slashCommandWorkspace) SidekickDashboardSubscribe(context.Context) <-chan pubsub.Event[agenttools.SidekickSurface] {
	return nil
}
func (w *slashCommandWorkspace) SidekickModel() config.SelectedModel         { return config.SelectedModel{} }
func (w *slashCommandWorkspace) SidekickSetModel(config.SelectedModel) error { return nil }

func (w *mcpPromptWorkspace) SidekickRun(context.Context, string) error { return nil }
func (w *mcpPromptWorkspace) SidekickCancel()                           {}
func (w *mcpPromptWorkspace) SidekickIsBusy() bool                      { return false }
func (w *mcpPromptWorkspace) SidekickClear(context.Context) error       { return nil }
func (w *mcpPromptWorkspace) SidekickSubscribe(context.Context) <-chan pubsub.Event[message.Message] {
	return nil
}

func (w *mcpPromptWorkspace) SidekickDashboardSubscribe(context.Context) <-chan pubsub.Event[agenttools.SidekickSurface] {
	return nil
}
func (w *mcpPromptWorkspace) SidekickModel() config.SelectedModel         { return config.SelectedModel{} }
func (w *mcpPromptWorkspace) SidekickSetModel(config.SelectedModel) error { return nil }
