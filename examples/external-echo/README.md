# External echo plugin

This is a small hands-side Go plugin using `plugins/external/sdk`.
Build it next to the manifest and activate it explicitly:

```sh
go build -o pons-plugin-echo .
pons --workspace . --plugin ./plugin.json
```

The executable speaks JSON-RPC 2.0/NDJSON on stdout and logs only to stderr.
Its `echo_text` action accepts typed `text`, `loud`, and `repeat` arguments.
The manifest is not discovered automatically.
