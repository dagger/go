// This go.mod has no module declaration on purpose.
//
// modfile parses it; no Go command accepts it. The scan records the reason
// against this module alone and carries on, so every other module in the
// workspace still gets checked. gomod's suite asserts both halves of that.

go 1.26.1
