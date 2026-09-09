// Package api embeds the OpenAPI specification for the JSON API.
package api

import _ "embed"

// openapiSpec is the OpenAPI 3.1 description of the /api/v1 surface. The
// route-coverage test walks the live chi router against it, so a route
// added without a spec entry — or a spec entry with no route — fails the
// build instead of drifting.
//
//go:embed openapi.json
var openapiSpec []byte
