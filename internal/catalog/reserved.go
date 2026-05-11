package catalog

// ReservedKeys lists request-body fields owned by the loop.
var ReservedKeys = []string{
	"model",
	"messages",
	"tools",
	"tool_choice",
	"stream",
	"stream_options",
	"max_tokens",
	"max_completion_tokens",
	"n",
}

// CheckReservedKeys returns reserved keys present in body, in ReservedKeys order.
func CheckReservedKeys(body map[string]any) []string {
	if len(body) == 0 {
		return nil
	}
	var found []string
	for _, key := range ReservedKeys {
		if _, ok := body[key]; ok {
			found = append(found, key)
		}
	}
	return found
}
