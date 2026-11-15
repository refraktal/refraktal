package safegit

import "testing"

// git lfs install in several scopes repeats identical values; that is still
// the standard driver. Any differing value disqualifies it.
func TestStandardLFSAcceptsRepeatedStandardValues(t *testing.T) {
	std := func(n int) map[string][]driverValue {
		d := map[string][]driverValue{}
		for kind, command := range lfsCommands {
			for range n {
				d["filter.lfs."+kind] = append(d["filter.lfs."+kind], driverValue{command, true})
			}
		}
		return d
	}
	if !standardLFS(std(1)) || !standardLFS(std(3)) {
		t.Fatal("standard LFS values, once or repeated, were rejected")
	}
	mixed := std(2)
	mixed["filter.lfs.smudge"][1] = driverValue{"evil smudge -- %f", true}
	if standardLFS(mixed) {
		t.Fatal("a differing repeated value was accepted")
	}
	none := std(1)
	delete(none, "filter.lfs.process")
	if standardLFS(none) {
		t.Fatal("a missing command was accepted")
	}
}
