package version

//go:generate sh -c "if test -f ../later/generated.go; then sed 's/package later/package version/' ../later/generated.go > generated.go; else printf 'package version\\n' > generated.go; fi"
