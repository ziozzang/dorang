-- pii_mask.lua — the worked example of DESIGN §10.5b.
--
-- It masks every text segment of a request with the pattern set the operator
-- configured on the model, and it does nothing else. The interesting parts are
-- deliberately NOT here:
--
--   * The mask table, the placeholder derivation and every unmasking pass are
--     the host's. This plugin never sees a table and never touches a response
--     frame, because the table is the object §10.5b calls the most dangerous in
--     the system and this file is untrusted code that outlives the request.
--   * The pattern set is configuration, not code. `patterns: [krrn, email]` on
--     the model decides what `dorang.mask` looks for, so an operator adds a
--     jurisdiction without editing this file.
--
-- What is left for a plugin is the judgement a regular expression cannot make:
-- *which* segments to mask. This one masks all of them, and shows how to skip a
-- class of segment, because a system prompt is written by the operator and
-- masking it usually costs the model context for no gain.
--
-- Copy it, change it, and point `filters.plugins[].path` at your copy.

dorang.require_api(1)

local cfg = dorang.config or {}

-- `skip_system: "true"` in the plugin's config leaves system prompts alone.
local skip_system = cfg.skip_system == "true"

-- `min_length` skips segments too short to be worth scanning. It is a cost
-- control, not a policy: a two-character segment cannot hold a resident
-- registration number.
local min_length = tonumber(cfg.min_length or "0") or 0

dorang.register("on_filter_request", function(req)
    local masked = 0
    local scanned = 0

    for i = 1, req.count do
        local label = req.doc.label(i)
        local skip = skip_system and label:sub(1, 6) == "system"

        if not skip then
            local text = req.doc.text(i)
            if #text >= min_length and #text > 0 then
                scanned = scanned + 1
                -- The host applies the model's configured pattern set, escapes
                -- anything placeholder-shaped the caller sent, and records the
                -- reverse mapping. An error here is not caught: there is no
                -- pcall in the sandbox, and a mask that did not happen must
                -- fail the request closed rather than be swallowed by a plugin.
                local out, n = dorang.mask(text)
                if n > 0 then
                    req.doc.set_text(i, out)
                    masked = masked + n
                end
            end
        end
    end

    -- Tags are counts and names. Putting a masked value in one would write key
    -- material into the trace, which is the whole thing §10.5b forbids.
    if masked > 0 then
        dorang.tag("pii_masked", tostring(masked))
    end
end)
