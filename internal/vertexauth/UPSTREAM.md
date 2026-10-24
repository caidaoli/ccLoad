# Vertex provenance

- Repository: `https://github.com/caidaoli/CLIProxyAPI`
- Reference commit: `f0f13cfda531da02a077748512f78a07679efd08`
- License: CLIProxyAPI MIT (`LICENSE`)

| ccLoad | Upstream |
| --- | --- |
| `keyutil.go` | `internal/auth/vertex/keyutil.go` (ported verbatim in behavior) |
| `credential.go` | `internal/auth/vertex/vertex_credentials.go`, `internal/api/handlers/management/vertex_import.go` |
| `token.go` | `google.CredentialsFromJSON` + `oauth2/jwt` JWT bearer grant used by `internal/runtime/executor/gemini_vertex_executor.go` |

Adopted from upstream `gemini_vertex_executor.go`: the service account
`publishers/google` URL layout, the `global` host without a location prefix,
the Regional Endpoint hosts (`aiplatform.{us,eu}.rep.googleapis.com`) for the
`us`/`eu` multi-region locations (upstream `88732ad5`),
the default `us-central1` location, `alt=sse` on streaming requests, and the
removal of `functionCall.id`/`functionResponse.id` for OpenAI Responses
sources. ccLoad routes by the channel URL instead of the credential, so the
credential `location` only seeds the URL of a newly created channel. Upstream
always translates and therefore always streams SSE; ccLoad adds `alt=sse` only
to streams it builds and leaves a Gemini passthrough query as the caller sent it.
Every upstream Vertex request passes `ConvertGeminiRequestToGemini`, which fills
missing content roles that Vertex rejects; ccLoad applies the same role rule to
publisher-layout bodies, including Gemini passthrough.

Model listing follows sub2api `4d9ac3e482106d2da3a39fe3e546d8d6801f1cc1`
(`backend/internal/service/upstream_models.go`, `listVertexGeminiPublisherModels`):
the paginated `v1beta1/publishers/google/models` list on the channel host,
filtered to `gemini-*`, with the embedded catalog as fallback.

Not adopted: Imagen `predict`, the Interactions API, Vertex API-key `config`
entries (a channel whose URL ends in `/publishers/google` with an API key
covers that mode), and `golang.org/x/oauth2` (the bearer grant is implemented
directly to avoid the dependency).
