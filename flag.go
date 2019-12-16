package lung

import (
	"github.com/gottingen/gekko/gflag"
)


type FlagValueSet interface {
	VisitAll(fn func(FlagValue))
}


type FlagValue interface {
	HasChanged() bool
	Name() string
	ValueString() string
	ValueType() string
}


type gflagValueSet struct {
	flags *gflag.FlagSet
}

func (p gflagValueSet) VisitAll(fn func(flag FlagValue)) {
	p.flags.VisitAll(func(flag *gflag.Flag) {
		fn(gflagValue{flag})
	})
}

// gflagValue is a wrapper aroung *gflag.flag
// that implements FlagValue
type gflagValue struct {
	flag *gflag.Flag
}

// HasChanged returns whether the flag has changes or not.
func (p gflagValue) HasChanged() bool {
	return p.flag.Changed
}

// Name returns the name of the flag.
func (p gflagValue) Name() string {
	return p.flag.Name
}

// ValueString returns the value of the flag as a string.
func (p gflagValue) ValueString() string {
	return p.flag.Value.String()
}

// ValueType returns the type of the flag as a string.
func (p gflagValue) ValueType() string {
	return p.flag.Value.Type()
}

