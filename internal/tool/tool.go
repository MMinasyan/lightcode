// Package tool contains the concrete Lightcode tools and compatibility aliases
// for the engine-owned generic tool contracts.
package tool

import (
	enginetool "github.com/MMinasyan/lightcode/internal/engine/tool"
)

type Tool = enginetool.Tool
type ArgumentNormalizer = enginetool.ArgumentNormalizer
type DisplayMetadataProvider = enginetool.DisplayMetadataProvider
type StageableTool = enginetool.StageableTool
type ToolCall = enginetool.ToolCall
type PendingExecutor = enginetool.PendingExecutor
type PendingCoordinator = enginetool.PendingCoordinator
type Registry = enginetool.Registry
type StagedCall = enginetool.StagedCall
type PendingQueue = enginetool.PendingQueue
type BatchResult = enginetool.BatchResult
type DefaultPendingCoordinator = enginetool.DefaultPendingCoordinator
type ExitError = enginetool.ExitError

var ErrDenied = enginetool.ErrDenied
var NewRegistry = enginetool.NewRegistry
var NewPendingQueue = enginetool.NewPendingQueue
