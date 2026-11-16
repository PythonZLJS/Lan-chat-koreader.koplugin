local M = {}

local function shell_quote(value)
    return "'" .. tostring(value):gsub("'", "'\\''") .. "'"
end

local function valid(path, kind)
    local file = io.open(path, "rb")
    if not file then return false end
    local first = file:read(8) or ""
    local size = file:seek("end") or 0
    file:close()
    if kind == "binary" then
        return first:sub(1, 4) == "\127ELF" and size > 1024 * 1024
    end
    return size > 32
end

local function copy_atomic(source, target)
    local input = io.open(source, "rb")
    if not input then return nil, "missing recovery file: " .. source end
    local temporary = target .. ".repair"
    local output = io.open(temporary, "wb")
    if not output then
        input:close()
        return nil, "cannot create repair file: " .. target
    end
    while true do
        local chunk = input:read(8192)
        if not chunk then break end
        local ok, err = output:write(chunk)
        if not ok then
            input:close()
            output:close()
            os.remove(temporary)
            return nil, "cannot write repair file: " .. tostring(err)
        end
    end
    input:close()
    output:flush()
    output:close()
    os.remove(target)
    if not os.rename(temporary, target) then
        os.remove(temporary)
        return nil, "cannot activate repaired file: " .. target
    end
    return true
end

local function verify_manifest(plugin_dir)
    local probe = io.popen("command -v sha256sum >/dev/null 2>&1 && (cd "
        .. shell_quote(plugin_dir) .. " && sha256sum -c payload.sha256 2>/dev/null)", "r")
    if not probe then return nil end
    local result = probe:read("*a") or ""
    local ok = probe:close()
    return ok and result:find("OK", 1, true) ~= nil
end

function M.ensure(plugin_dir)
    local required = {
        {"client.lua", "lua"},
        {"runtime.sh", "shell"},
        {"bin/neighbour-chat", "binary"},
    }
    local recovery = plugin_dir .. "/recovery"
    local manifest_ok = verify_manifest(plugin_dir)
    for _, entry in ipairs(required) do
        local relative, kind = entry[1], entry[2]
        local target = plugin_dir .. "/" .. relative
        if not valid(target, kind) or manifest_ok == false then
            local source = recovery .. "/" .. relative
            local ok, err = copy_atomic(source, target)
            if not ok then
                return nil, "Neighbour Chat is incomplete (" .. relative .. "): " .. tostring(err)
            end
        end
        if not valid(target, kind) then
            return nil, "Neighbour Chat could not verify repaired file: " .. relative
        end
    end
    if verify_manifest(plugin_dir) == false then
        return nil, "Neighbour Chat package verification failed."
    end
    return true
end

return M
