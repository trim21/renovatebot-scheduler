package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Run is one Renovate execution, including the captured log.
type Run struct {
	ent.Schema
}

func (Run) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			Comment("run id"),
		field.String("repo"),
		field.String("trigger"),
		field.Int("attempt"),
		field.Time("started_at"),
		field.Time("finished_at").
			Optional().
			Comment("unset while the run is still in progress"),
		field.Int("exit_code").
			Default(-1),
		field.String("error").
			Default(""),
		field.Bytes("log").
			Optional().
			Comment("captured stdout and stderr of the run"),
	}
}

func (Run) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("repo"),
		index.Fields("started_at"),
	}
}
