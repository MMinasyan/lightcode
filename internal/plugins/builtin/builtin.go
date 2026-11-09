// Package builtin assembles MELTA's shipped optional native extension
// registration set: the bundled model adaptation and the Agents extension
// (definitions, configuration edit, and child-session task tool). The durable
// SQLite store, the jobs service, and the seven standard tools are baseline
// services the MELTA owner constructs directly, so they are not part of
// this set. It is ordinary composition data with no configuration logic and
// no additional Core authority; a custom build supplies its own native slice
// through the same melta.Open and its own Agent constructor worlds.
package builtin

import (
	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extensions/adaptation"
	"github.com/MMinasyan/lightcode/extensions/agents"
)

// Plugins returns the shipped registration set in registration order.
func Plugins() []extension.Native {
	adaptationPlugin := adaptation.Native()
	// The shipped Agent constructor universe is the complete registered set:
	// the eight standard tool capabilities (the seven baseline tools and the
	// Agents task tool), the bundled adaptation hook, and the Agents
	// extension's two services. It is construction data, not a product
	// default discovered by the Runtime.
	universe := append([]string(nil), agents.StandardTools...)
	universe = append(universe, adaptation.CapabilityID, agents.DefinitionsCapabilityID, agents.ConfigurationCapabilityID)
	return []extension.Native{
		adaptationPlugin,
		agents.Native(agents.Options{
			CapabilityIDs:        universe,
			ToolIDs:              agents.StandardTools,
			DefaultCapabilityIDs: DefaultCapabilityIDs(),
		}),
	}
}

// DefaultCapabilityIDs returns the shipped application's default capability
// selection: the bundled model adaptation contribution. A custom host supplies
// its own ordinary selection to its Agent extension constructor and its
// Doctor inputs.
func DefaultCapabilityIDs() []string {
	return []string{adaptation.CapabilityID}
}
