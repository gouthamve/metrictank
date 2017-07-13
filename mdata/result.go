package mdata

import (
	"github.com/raintank/metrictank/mdata/chunk"
	"gopkg.in/raintank/schema.v1"
)

type Result struct {
	Points []schema.Point
	Iters  []chunk.Iter
	Oldest uint32
}
