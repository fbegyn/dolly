# dolly

A Prometheus alerts dashboard generator. It queries a Prometheus server and renders a single self-contained HTML page showing every alerting rule as a hexagon, per instance, coloured by state. It can write the file once, keep regenerating it, or serve it over HTTP.

## How it works

1. Fetches active targets (`/api/v1/targets`) so all target labels are available for grouping.
2. Fetches alerting rules (`/api/v1/rules`).
3. For each rule, emits one hexagon per instance:
   - **firing** / **pending**: the alert is currently active on that instance.
   - **present**: the metrics used by the rule exist on that instance but the alert is not active (i.e. OK). This is found by parsing the rule's PromQL expression and querying for its vector selectors.
4. Renders `alerts.html.tmpl` (embedded in the binary) into the output file. Writes are atomic (temp file + rename).

The page uses a colour-blind-friendly palette and supports light and dark mode.

## Requirements

- Go 1.18+
- A reachable Prometheus server

## Build

```sh
go build -o dolly .
```

## Usage

```sh
# Generate alerts.html once
./dolly

# Regenerate every 30s; the page reloads itself at the same interval
./dolly --refresh.interval=30s

# Regenerate periodically and serve at http://localhost:8080/
./dolly serve --web.listen-address=:8080 --refresh.interval=30s

# Group by several labels, hide OK hexagons
./dolly --group.by=job,datacenter --hide.inactive
```

### Commands

| Command    | Description                                                              |
|------------|--------------------------------------------------------------------------|
| `generate` | (default) Write the HTML file, once or periodically.                      |
| `serve`    | Regenerate the file periodically and serve it over HTTP.                  |

### Flags

| Flag                   | Default      | Description                                                                         |
|------------------------|--------------|-------------------------------------------------------------------------------------|
| `--output.file`        | `alerts.html`| HTML output file.                                                                    |
| `--refresh.interval`   | `0s`         | Regenerate and reload at this interval. `0` generates once (`serve` uses 30s then). |
| `--group.by`           | `job`        | Comma-separated labels to group hexagons by, outermost first.                        |
| `--hide.inactive`      | `false`      | Hide hexagons for alerts that are neither firing nor pending.                        |
| `--web.listen-address` | `:8080`      | Listen address (`serve` only).                                                       |

Prometheus connection flags (URL, authentication, TLS, logging) are provided by [oy-toolkit](https://github.com/o11ydev/oy-toolkit). Run `./dolly --help` to see them all.

## Project layout

- `main.go`: CLI, Prometheus queries, PromQL analysis, report writer, HTTP server.
- `alerts.html.tmpl`: HTML/CSS/JS template for the dashboard.

## License

Apache License 2.0. See [LICENSE](LICENSE).
