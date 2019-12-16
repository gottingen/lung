package lung

import (
	"testing"

	"github.com/gottingen/gekko/gflag"
	"github.com/stretchr/testify/assert"
)

func TestBindFlagValueSet(t *testing.T) {
	flagSet := gflag.NewFlagSet("test", gflag.ContinueOnError)

	var testValues = map[string]*string{
		"host":     nil,
		"port":     nil,
		"endpoint": nil,
	}

	var mutatedTestValues = map[string]string{
		"host":     "localhost",
		"port":     "6060",
		"endpoint": "/public",
	}

	for name := range testValues {
		testValues[name] = flagSet.String(name, "", "test")
	}

	flagValueSet := gflagValueSet{flagSet}

	err := BindFlagValues(flagValueSet)
	if err != nil {
		t.Fatalf("error binding flag set, %v", err)
	}

	flagSet.VisitAll(func(flag *gflag.Flag) {
		flag.Value.Set(mutatedTestValues[flag.Name])
		flag.Changed = true
	})

	for name, expected := range mutatedTestValues {
		assert.Equal(t, Get(name), expected)
	}
}

func TestBindFlagValue(t *testing.T) {
	var testString = "testing"
	var testValue = newStringValue(testString, &testString)

	flag := &gflag.Flag{
		Name:    "testflag",
		Value:   testValue,
		Changed: false,
	}

	flagValue := gflagValue{flag}
	BindFlagValue("testvalue", flagValue)

	assert.Equal(t, testString, Get("testvalue"))

	flag.Value.Set("testing_mutate")
	flag.Changed = true //hack for gflag usage

	assert.Equal(t, "testing_mutate", Get("testvalue"))
}
