# gtav-glb-to-ydr

A small command-line tool that converts a **GLB** (glTF binary) model into a ready-to-use
GTA V / FiveM **drawable** package (a `.ydr` plus its `.ytyp` archetype and a `.ytd` texture
dictionary, and a `.ycd` clip when the GLB has animation), using the gtax.dev API. It can also
inspect a GLB or render a quick PNG preview without spending any convert quota.


```
gtav-glb-to-ydr -i model.glb                       # convert -> model.zip
gtav-glb-to-ydr -i model.glb -format fivem-zip     # FiveM stream resource
gtav-glb-to-ydr -i model.glb -inspect              # mesh/material/animation metadata (active plan)
gtav-glb-to-ydr -i model.glb -render -o p.png      # isometric PNG preview (active plan)
```

## Example

Preview of the example model (`office_chair.glb`, in [`example/`](example)) converted with this tool:

<video src="https://github.com/gtax-dev/gtav-glb-to-ydr/raw/main/example/example.mp4" controls muted width="100%"></video>

If the video does not play inline, [view example.mp4](example/example.mp4).

```bash
gtav-glb-to-ydr -i example/office_chair.glb -object-name office_chair
```

## Install

Download a prebuilt binary from the [Releases](https://github.com/gtax-dev/gtav-glb-to-ydr/releases)
page (linux / macOS / windows, amd64 / arm64), unpack it, and put it on your `PATH`.

From source (Go 1.21+):

```bash
go install github.com/gtax/gtav-glb-to-ydr@latest
```

## Authentication

Converting needs your global gtax.dev API key (`gtax_dglb_...`), the same key the other gtax tools use.
Get one at [gtax.dev/api-keys](https://gtax.dev/api-keys) and provide it via the environment (preferred)
or the `-api-key` flag:

```bash
export GTAX_API_KEY=gtax_dglb_xxxxxxxx
gtav-glb-to-ydr -i model.glb
```

`-inspect` requires a key on an active Basic/Pro/Ultra plan (it does not count against your daily
convert quota). `-render` likewise requires a key on an active plan and consumes no convert quota.

| Daily convert limit | Plan |
|---------------------|------|
| 3 / day | basic |
| 100 / day | Pro |
| 300 / day | Ultra |

## Usage

### Convert (default)

```bash
gtav-glb-to-ydr -i model.glb [options]
```

| Flag | Default | Notes |
|------|---------|-------|
| `-i` | (required) | input `.glb` file |
| `-o` | (derived) | output path; defaults to the server's result name, else `<object-name>.zip` |
| `-object-name` | `model` | output name stem (e.g. `model.ydr`) |
| `-format` | `zip` | `zip`, `dlc-rpf` (singleplayer DLC archive), or `fivem-zip` (stream resource) |
| `-scale` | `1.0` | uniform model scale |
| `-lod-med`, `-lod-low`, `-lod-vlow` | `0` | LOD switch distances (`0` = that LOD is skipped) |
| `-archetype-lod-distance` | (auto) | override the archetype LOD distance |
| `-archetype-hd-texture-distance` | (auto) | override the archetype HD texture distance |
| `-archetype-flags` | (auto) | override the archetype flags (uint32) |
| `-texture-mode` | `ytd` | `ytd` (separate `.ytd`) or `embed` (bake textures into the YDR) |
| `-include-animations` | `false` | extract the GLB's animations into a `.ycd` |

```bash
# A FiveM resource named "pain", with animations baked in
gtav-glb-to-ydr -i pain.glb -object-name pain -format fivem-zip -include-animations
```

### Inspect

Prints the GLB's geometry / material / animation summary as JSON, without converting. Requires a key
on an active Basic/Pro/Ultra plan; it consumes no convert quota:

```bash
gtav-glb-to-ydr -i model.glb -inspect
```

```jsonc
{
  "mesh_count": 1,
  "total_vertices": 4123,
  "total_triangles": 7890,
  "materials": [{ "index": 0, "name": "Crust", "texture_name": "pain_diffuse" }],
  "textures": ["pain_diffuse"],
  "has_animations": false,
  "bounding_box": { "min": [-0.5, 0, -0.5], "max": [0.5, 1.2, 0.5] }
}
```

### Render

Writes an isometric PNG preview. Requires a key on an active Basic/Pro/Ultra plan; consumes no quota:

```bash
gtav-glb-to-ydr -i model.glb -render -size 1024 -o preview.png
```

| Flag | Default | Notes |
|------|---------|-------|
| `-size` | `768` | square output size in pixels, clamped to 64..2048 |

## Common options

| Flag | Default | Notes |
|------|---------|-------|
| `-api-key` | (env) | API key, or set `GTAX_API_KEY` (also reads `GLB_API_KEY`) |
| `-api-url` | (default) | API base URL, or set `GTAX_API_URL` (default `https://public-glb-to-ydr.gtax.dev`) |
| `-timeout` | `5m` | HTTP client timeout |
| `-quiet` | `false` | suppress progress on stderr |
| `-version` | | print the version and exit |

Uploads are capped at 100 MB (the hosted API sits behind Cloudflare). On an error the tool prints the
server's `{"error":"..."}` message and the rate-limit headers, and exits non-zero. The written output
path is printed to stdout; progress and diagnostics go to stderr.

## License

MIT, see [LICENSE](LICENSE).
