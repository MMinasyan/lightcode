package catalog

import "github.com/MMinasyan/lightcode/model"

// CheckReservedKeys returns reserved keys present in body, in the model-owned
// order. The reserved set has one owner — the encoder's base body — so
// configuration validation and transport acceptance can never disagree about
// which keys are owned.
func CheckReservedKeys(body map[string]any) []string {
	if len(body) == 0 {
		return nil
	}
	var found []string
	for _, key := range model.ReservedBodyKeys() {
		if _, ok := body[key]; ok {
			found = append(found, key)
		}
	}
	return found
}
