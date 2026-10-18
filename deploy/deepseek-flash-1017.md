# DeepSeek V4.1 Flash release pin (2026-10-18)

Ollama Pro lists both the rolling id and `deepseek-v4.1-flash:1017`.
Both passed `dorangctl catalog verify`; use the dated id for Ollama deployments.
Keep the public group `deepseek-v4.1-flash` and its existing
`deepseek-v4.1-flash:cloud` and `deepseek-v4-flash` aliases.

In the operator configuration, replace only the two Ollama deployments'
`upstream_model: "deepseek-v4.1-flash"` with
`upstream_model: "deepseek-v4.1-flash:1017"`.
Preserve both credentials, priorities, and the Qwen fallback model.
Install the verification overlay in the catalog directory as a separate file,
so scheduled catalog sync does not overwrite it.

Roll through the established Hangang :14000 blue/green runner: validate the
inactive member, switch with CAS, smoke the frontend, and drain the previous
member. Never recreate the active member. The existing binary accepts the new
model through the external catalog overlay, so no binary rebuild is required.
