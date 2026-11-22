package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// Repo is a repository whose webhook this service manages and whose events
// trigger Renovate.
type Repo struct {
	ent.Schema
}

func (Repo) Fields() []ent.Field {
	return []ent.Field{
		field.String("full_name").
			Unique().
			Comment("owner/repo"),
		field.String("default_branch"),
		field.Int64("hook_id"),
		field.String("hook_token").
			Unique(),
		field.String("enabled_by"),
		field.Time("enabled_at").
			Default(time.Now),
	}
}
