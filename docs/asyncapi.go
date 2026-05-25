package docs

import _ "embed"

// AsyncAPIYAML is the committed WebSocket contract served by the API.
//
//go:embed asyncapi.yaml
var AsyncAPIYAML []byte
