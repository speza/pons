# External policy hook example

Build and install the example under its manifest ID:

```sh
go build -o examples/external-policy/external-policy ./examples/external-policy
mkdir -p ~/.pons/plugins/example.policy
cp examples/external-policy/plugin.json examples/external-policy/external-policy ~/.pons/plugins/example.policy/
```

Add this entry to the global `~/.pons/config.json` `plugins` list:

```json
{"id":"example.policy","version":"1.0.0","enabled":true,"config":{}}
```

The example requests approval for `bash` and `shell` actions. Without an
approval handler, Pons denies those calls. It is intentionally simple: the
external protocol supports `on_tool_call_start` decisions, while any real
policy needs to assess the user request, recent context, tool resources, and
execution environment.
