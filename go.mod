module github.com/dleiferives/audio-server

go 1.26

require (
	github.com/dleiferives/MFA-go v0.0.0-00010101000000-000000000000
	github.com/gorilla/websocket v1.5.3
	golang.org/x/text v0.40.0
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/dleiferives/MFA-go => ./mfa-go
