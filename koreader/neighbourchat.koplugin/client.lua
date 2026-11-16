local json = require("json")
local Client = {}
Client.__index = Client

local function quote(value)
    return "'" .. value:gsub("'", "'\\''") .. "'"
end

-- No global LuaSocket timeout changes, proxies or external HTTP in KOReader.
-- A single loopback request has both a bounded size and an absolute deadline.
local function transport(method, path, body, token, etag)
    local ok_socket, socket = pcall(require, "socket")
    if not ok_socket or not socket then return nil, "Local networking is unavailable." end
    local tcp, err = socket.tcp()
    if not tcp then return nil, err end
    local deadline = socket.gettime() + 0.7
    tcp:settimeout(0.25, "b")
    tcp:settimeout(0.6, "t")
    local function fail(message) tcp:close(); return nil, message end
    local ok = tcp:connect("127.0.0.1", 8001)
    if not ok then return fail("Chat is not running.") end
    body = body or ""
    local auth = token and ("Authorization: Bearer " .. token .. "\r\n") or ""
    if etag then auth=auth.."If-None-Match: "..etag.."\r\n" end
    ok = tcp:send(method .. " " .. path .. " HTTP/1.0\r\nHost: 127.0.0.1:8001\r\n" .. auth
        .. "Content-Type: application/json\r\nContent-Length: " .. #body .. "\r\nConnection: close\r\n\r\n" .. body)
    if not ok then return fail("The local chat service is busy.") end
    local line = tcp:receive("*l")
    local status = line and tonumber(line:match("^HTTP/%d%.%d (%d%d%d)"))
    if not status then return fail("Invalid local chat response.") end
    local headers, length, response_etag = 0, nil, nil
    repeat
        line = tcp:receive("*l")
        if not line then return fail("Incomplete local chat response.") end
        headers = headers + #line
        if headers > 16384 or socket.gettime() > deadline then return fail("Local response limit exceeded.") end
        local value = line:lower():match("^content%-length:%s*(%d+)")
        if value then length = tonumber(value) end
        local tag=line:match('^[Ee][Tt][Aa][Gg]:%s*("[%d%-a-z]+")%s*$')
        if tag then response_etag=tag end
    until line == ""
    if length and length > 524288 then return fail("Local response too large.") end
    if status==304 then tcp:close();return status,"",response_etag end
    local chunks, total = {}, 0
    repeat
        local data, receive_error, partial = tcp:receive(math.min(4096, length and math.max(1, length-total) or 4096))
        local chunk = data or partial or ""
        total = total + #chunk
        if total > 524288 or socket.gettime() > deadline then return fail("Local response limit exceeded.") end
        chunks[#chunks+1] = chunk
        if length and total >= length then break end
        if receive_error then
            if receive_error ~= "closed" then return fail("Chat response timed out.") end
            break
        end
    until false
    tcp:close()
    if length and total ~= length then return nil, "Incomplete local response." end
    return status, table.concat(chunks), response_etag
end

function Client.new(plugin_dir, options)
    options = options or {}
    return setmetatable({path=plugin_dir, request=options.transport or transport,
        token_file=options.token_file or plugin_dir .. "/data/koreader-token.txt",
        launch=options.launch, clock=options.clock or os.time, state={stage="idle"}}, Client)
end

function Client:call(method, path, data, public)
    if type(method) ~= "string" or type(path) ~= "string" then return nil, "Invalid local action." end
    if path ~= "/_health" and path ~= "/_reader/state" and path ~= "/_reader/action" then return nil,"Invalid local action." end
    if not public and not self.token then
        local file = io.open(self.token_file, "rb")
        if file then self.token=file:read(128);file:close() end
        if self.token then self.token=self.token:match("^%s*([A-Za-z0-9_-]+)%s*$") end
        if not self.token or #self.token ~= 43 then self.token=nil;return nil,"Chat is still starting." end
    end
    local encoded, encode_error
    if data ~= nil then
        local encoded_ok, value = pcall(json.encode, data)
        if not encoded_ok or type(value) ~= "string" then
            encode_error = "Could not encode the local chat action."
        else
            encoded = value
        end
    end
    if encode_error then return nil, encode_error end
    local ok, code, body, etag = pcall(self.request, method, path, encoded, public and nil or self.token,
        path=="/_reader/state" and self.etag or nil)
    if not ok or not code then return nil, body or "The local chat service is not responding." end
    if code==304 and path=="/_reader/state" then return self.state end
    local decoded, value = pcall(json.decode, body or "")
    if not decoded or type(value)~="table" then return nil,"Invalid local response." end
    if code < 200 or code >= 300 then return nil,value.error or "Chat operation failed." end
    if path=="/_reader/state" then self.etag=etag end
    return value
end

function Client:health()
    local info, err = self:call("GET","/_health",nil,true)
    if not info then return nil,err end
    if info.service~="neighbour-chat-v3" or info.reader_api~=4 then
        return nil,"An older app or another service is using the chat port. Stop chat and restart the Kindle before opening this plugin."
    end
    return true
end

function Client:start()
    local healthy, err = self:health()
    if healthy then self.starting=nil;return true end
    -- Do not replace or kill a service that answered on this port.
    local info = self:call("GET","/_health",nil,true)
    if info then return nil,err end
    if not self.starting then
        self.starting=self.clock()+50
        if self.launch then self.launch() else
            os.execute("nohup /bin/sh "..quote(self.path.."/runtime.sh").." </dev/null >/dev/null 2>&1 &")
        end
    elseif self.clock()>self.starting then
        self.starting=nil
        return nil,"Chat could not start. Check data/NeighbourChat.log or restart the Kindle."
    end
    return false,"Starting chat..."
end

function Client:readState()
    local state, err = self:call("GET","/_reader/state")
    if state and type(state) == "table" then self.state=state
    elseif state then return nil, "The local chat returned invalid state." end
    return state,err
end

function Client:action(action)
    if type(action) ~= "table" or type(action.action) ~= "string" then return nil, "Invalid chat action." end
    if action.context==nil then action.context=self.state.context end
    return self:call("POST","/_reader/action",action)
end

function Client:stop()
    local result,err=self:action{action="stop"}
    if result then self.token=nil;self.etag=nil;self.starting=nil;self.state={stage="idle"} end
    return result,err
end

return Client
