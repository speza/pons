# External TypeScript echo plugin

This example uses the dependency-free SDK in
`plugins/external/sdk/typescript`. Build it with an installed Node and
TypeScript compiler:

```sh
rm -rf .build
bun build plugins/external/sdk/typescript/index.ts \
  examples/external-echo-ts/index.ts --outdir .build --target node
# Alternatively use a locally installed TypeScript compiler with equivalent
# NodeNext/ES2022 options.
```

Activate it explicitly. The host keeps the child environment empty except for
the configured runtime path, so provide the directory containing `node`:

```sh
pons --plugin examples/external-echo-ts/plugin.json \
  --plugin-path /usr/local/bin:/usr/bin:/bin
```

On Homebrew Apple Silicon, include `/opt/homebrew/bin` in `--plugin-path`.
The manifest is never discovered automatically.
