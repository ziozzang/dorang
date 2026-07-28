# `catalog.d` — one file per provider

An example of the directory layer. Point dorang at this directory and every
`*.yaml` / `*.yml` in it is loaded, sorted by name, each overlaying the last:

    catalog.Load("/etc/dorang/catalog.d")
    DORANG_CATALOG_PATH=/etc/dorang/catalog.d dorangctl serve
    dorangctl lint /etc/dorang/catalog.d

The numeric prefixes are the whole ordering mechanism — `10-` is applied
before `20-`, exactly as a directory listing reads. Subdirectories are not
descended, so a `disabled/` subdirectory is an off switch.

`10-acme.yaml` is a provider this build of dorang has never heard of,
described entirely in configuration. `20-corrections.yaml` shows the two ways
a later file changes an earlier one, and which of them can remove a value.

Check what a layer actually did, per field, with `Catalog.Explain`:

    for _, o := range c.Explain("acme", "acme-lodestar-2") {
        fmt.Println(o)   // context_window=190000 from file .../20-corrections.yaml layer=model
    }
