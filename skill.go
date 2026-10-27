// Package hypergraphia exposes files shipped inside the binary.
package hypergraphia

import _ "embed"

// SkillMD is the agent skill installed into ~/.claude/skills/hypergraphia.
//
//go:embed skills/hypergraphia/SKILL.md
var SkillMD string
