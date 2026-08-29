// Package tool contains the concrete Lightcode tools and compatibility aliases
// for the engine-owned generic tool contracts.
package tool

import (
	enginetool "github.com/MMinasyan/lightcode/internal/engine/tool"
)

type Tool = enginetool.Tool
type ArgumentNormalizer = enginetool.ArgumentNormalizer
type DisplayMetadataProvider = enginetool.DisplayMetadataProvider
type ToolCall = enginetool.ToolCall
type Registry = enginetool.Registry
type ExitError = enginetool.ExitError

var ErrDenied = enginetool.ErrDenied
var NewRegistry = enginetool.NewRegistry
