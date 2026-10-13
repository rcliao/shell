package mcp

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// maxExactJSONInt is 2^53: the largest integer a JavaScript number holds
// exactly.
const maxExactJSONInt = 1 << 53

// idArg is a chat or thread id tool argument. Discord snowflakes run past
// 2^53, and Claude Code parses tool arguments as JavaScript numbers, so a
// 19-digit id sent as a JSON number reaches this server already rounded
// (…489860 arrives as …489900) and the send goes to a channel that does not
// exist. A string keeps every digit; a number past 2^53 is refused rather than
// trusted, since there is no telling whether it was rounded on the way.
type idArg int64

func (a *idArg) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*a = 0
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		str = strings.TrimSpace(str)
		if str == "" {
			*a = 0
			return nil
		}
		n, err := strconv.ParseInt(str, 10, 64)
		if err != nil {
			return fmt.Errorf("id %q is not an integer", str)
		}
		*a = idArg(n)
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("id %s is not an exact integer; pass it as a string, e.g. \"1234\"", s)
	}
	if n > maxExactJSONInt || n < -maxExactJSONInt {
		return fmt.Errorf("id %d is too long to pass as a number (it may have been rounded on the way); pass it as a string, e.g. \"%d\"", n, n)
	}
	*a = idArg(n)
	return nil
}

// idProp is the schema for an idArg: a string, so models quote long ids.
func idProp(desc string) map[string]any {
	return prop("string", desc+" Pass it as a string (long Discord ids lose digits as numbers).")
}
