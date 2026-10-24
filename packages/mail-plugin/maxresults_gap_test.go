package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The person's maxResults is the most messages one command reads. A max:<n> in the instruction
// may lower it but must not raise it.
func TestListMaxWordCannotReadMoreThanThePersonsMaxResults(t *testing.T) {
	eachProvider(t, func(t *testing.T, box watchBox) {
		for i := range 10 {
			box.arrive(t, "a@example.org", fmt.Sprintf("Message %d", i), time.Now().Add(time.Duration(i)*time.Second), false)
		}
		for _, instr := range []string{"list max:50", "search subject:Message max:50"} {
			o := runIt(t, instr, opts{conn: box.conn(), maxResults: 3})
			mustOK(t, o)
			if n := strings.Count(o.output, " | snippet: "); n > 3 {
				t.Errorf("%s with maxResults 3 read %d messages", instr, n)
			}
		}
	})
}
