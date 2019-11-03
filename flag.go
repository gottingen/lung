package lung

import "github.com/spf13/pflag"


type FlagValueSet interface {
	VisitAll(fn func(FlagValue))
}


type FlagValue interface {
	HasChanged() bool
	Name() string
	ValueString() string
	ValueType() string
}


type pflagValueSet struct {
	flags *pflag.FlagSet
}

func (p pflagValueSet) VisitAll(fn func(flag FlagValue)) {
	p.flags.VisitAll(func(flag *pflag.Flag) {
		fn(pflagValue{flag})
	})
}

// pflagValue is a wrapper aroung *pflag.flag
// that implements FlagValue
type pflagValue struct {
	flag *pflag.Flag
}

// HasChanged returns whether the flag has changes or not.
func (p pflagValue) HasChanged() bool {
	return p.flag.Changed
}

// Name returns the name of the flag.
func (p pflagValue) Name() string {
	return p.flag.Name
}

// ValueString returns the value of the flag as a string.
func (p pflagValue) ValueString() string {
	return p.flag.Value.String()
}

// ValueType returns the type of the flag as a string.
func (p pflagValue) ValueType() string {
	return p.flag.Value.Type()
}

