-- Kindle LAN Chat: a small, self-hosted chat for KOReader on a trusted LAN.
-- Network traffic is plain HTTP. Do not use on an untrusted Wi-Fi network.
local WidgetContainer = require("ui/widget/container/widgetcontainer")
local UIManager = require("ui/uimanager")
local Menu = require("ui/widget/menu")
local InputDialog = require("ui/widget/inputdialog")
local InfoMessage = require("ui/widget/infomessage")
local ConfirmBox = require("ui/widget/confirmbox")
local DataStorage = require("datastorage")
local LuaSettings = require("luasettings")
local Device = require("device")
local logger = require("logger")
local _ = require("gettext")
local socket_ok, socket = pcall(require, "socket")
local json_ok, json = pcall(require, "json")

local Chat = WidgetContainer:extend{ name = "kindle_lan_chat" }
local MAX_BODY, MAX_RESPONSE, MAX_MESSAGES, MAX_CLIENTS, MAX_PENDING = 8192, 196608, 30, 8, 20
local PORT = 8765
local LOG_LIMIT = 256 * 1024

-- Deliberately do not write request bodies, passwords, join codes, message text,
-- or bearer tokens to this log. It is designed to be safe to share for support.
local function appendDebug(path, level, message)
    local f = io.open(path, "ab")
    if not f then return false end
    local ok = pcall(function()
        local wrote, write_err = f:write(os.date("!%Y-%m-%dT%H:%M:%SZ"), " [", level, "] ", tostring(message), "\n")
        if not wrote then error(write_err or "write failed") end
        local flushed, flush_err = f:flush()
        if not flushed then error(flush_err or "flush failed") end
    end)
    f:close()
    return ok
end

local function clean(s, limit)
    if type(s) ~= "string" then return "" end
    s = s:gsub("[%z\1-\8\11\12\14-\31\127]", "")
    if #s > limit then s = s:sub(1, limit) end
    return s
end

local function countKeys(t)
    local count = 0
    for _ in pairs(t or {}) do count = count + 1 end
    return count
end

function Chat:init()
    -- Kindle USB storage is mounted at /mnt/us; this file is directly visible
    -- from the top-level Kindle drive when connected over USB.
    self.log_file = "/mnt/us/kindle-lan-chat-debug.log"
    self.settings_file = DataStorage:getSettingsDir() .. "/kindle_lan_chat.lua"
    self.settings = LuaSettings:open(self.settings_file)
    self.settings.data.recent = self.settings.data.recent or {}
    self.users = self.settings.data.users or {}
    self.pending = {}
    self.failed_auth = {}
    self.messages = {}
    self.clients = {}
    self.running = false
    self.hosting = false
    self.discovery = false
    self.chat_hidden = true
    self.status = "Ready"
    self.next_id = 1
    local kindle_ok, is_kindle = pcall(function() return Device:isKindle() end)
    self:log("INFO", "Plugin started; Lua=" .. tostring(_VERSION)
        .. "; KOReader=" .. tostring(rawget(_G, "VERSION") or "unknown")
        .. "; LuaSocket=" .. tostring(socket_ok) .. "; JSON=" .. tostring(json_ok)
        .. "; Kindle=" .. tostring(kindle_ok and is_kindle or "unknown"))
    self.ui.menu:registerToMainMenu(self)
end

function Chat:log(level, message)
    local text = tostring(message):gsub("[%z\1-\8\11\12\14-\31\127]", " "):sub(1, 500)
    local lfs_ok, lfs = pcall(require, "libs/libkoreader-lfs")
    if lfs_ok then
        local attr = lfs.attributes(self.log_file, "size")
        if attr and attr > LOG_LIMIT then
            local reset = io.open(self.log_file, "wb")
            if reset then
                reset:write(os.date("!%Y-%m-%dT%H:%M:%SZ"), " [INFO] Debug log cleared after reaching its size limit.\n")
                reset:close()
            end
        end
    end
    local ok = appendDebug(self.log_file, level, text)
    self.log_available = ok
    if not ok and not self.log_error_warned then
        self.log_error_warned = true
        logger.warn("Kindle LAN Chat: unable to write debug log", self.log_file)
    end
end

function Chat:diagnostics()
    self:log("INFO", "Support snapshot: hosting=" .. tostring(self.hosting)
        .. "; connected=" .. tostring(self.running) .. "; discovery=" .. tostring(self.discovery)
        .. "; pending=" .. tostring(#self.pending) .. "; messages=" .. tostring(#self.messages)
        .. "; clients=" .. tostring(#self.clients) .. "; status=" .. tostring(self.status))
    if self.log_available == false then
        return self:notify(_("Could not write the debug log. Check that Kindle storage is mounted, then send me this path and the KOReader crash log:\n") .. self.log_file)
    end
    self:notify(_("Diagnostics recorded. Plug in the Kindle by USB and find this one file at the top level:\n") .. self.log_file
        .. _("\nIt excludes passwords, join codes, tokens, and message text."))
end

function Chat:addToMainMenu(menu_items)
    menu_items.kindle_lan_chat = {
        text = _("LAN Chat"),
        sorting_hint = "tools",
        callback = function() self:runAction("Open LAN Chat", function() self:showHome() end) end,
    }
end

function Chat:save()
    self.settings.data.users = self.users
    self.settings:flush()
end

function Chat:notify(text)
    UIManager:show(InfoMessage:new{ text = text })
end

function Chat:runAction(label, callback)
    self:log("INFO", "UI action: " .. tostring(label))
    local ok, err = xpcall(callback, debug.traceback)
    if not ok then
        self:log("ERROR", "UI action failed: " .. tostring(err))
        self:notify(_("That action failed. Create a support log entry and send me kindle-lan-chat-debug.log."))
    end
end

function Chat:showMenu(title, rows)
    for index, row in ipairs(rows) do
        if row.callback and not row._lan_chat_wrapped then
            local original = row.callback
            local item_index = index
            row.callback = function() self:runAction(tostring(title) .. " item " .. tostring(item_index), original) end
            row._lan_chat_wrapped = true
        end
    end
    self:log("INFO", "Opened menu: " .. tostring(title))
    UIManager:show(Menu:new{ title = title, item_table = rows })
end

function Chat:input(title, initial, callback)
    local dialog
    dialog = InputDialog:new{
        title = title, input = initial or "",
        buttons = {
            { { text = _("Cancel"), id = "close", callback = function() UIManager:close(dialog) end },
              { text = _("Continue"), is_enter_default = true, callback = function()
                  local v = dialog:getInputText(); UIManager:close(dialog)
                  local ok, err = xpcall(function() callback(v) end, debug.traceback)
                  if not ok then self:log("ERROR", "Input action failed: " .. tostring(err)); self:notify(_("That action failed. See kindle-lan-chat-debug.log.")) end
              end } },
        },
    }
    UIManager:show(dialog); dialog:onShowKeyboard()
end

function Chat:showHome()
    local rows = {
        { text = self.hosting and _("Host controls") or _("Host a chat on this Kindle"), callback = function() self:hostMenu() end },
        { text = _("Find nearby chats"), callback = function() self:scanChats() end },
        { text = _("Join by address"), callback = function() self:joinAddress() end },
        { text = self.running and _("Open chat") or _("Chat is not connected"), enabled = self.running, callback = function() self:chatMenu() end },
        { text = _("Recent hosts"), callback = function() self:recentMenu() end },
        { text = _("Create support log entry"), callback = function() self:diagnostics() end },
        { text = _("Show debug log file location"), callback = function() self:notify(self.log_file) end },
        { text = _("Close"), id = "close" },
    }
    local title = _("LAN Chat") .. " — " .. tostring(self.status):sub(1, 28)
    self:showMenu(title, rows)
end

function Chat:hostMenu()
    local rows = {
        { text = self.hosting and _("Show host address and codes") or _("Start hosting"), callback = function() if self.hosting then self:showHostInfo() else self:startHost() end end },
        { text = _("Join requests (%1)"):gsub("%%1", tostring(#self.pending)), callback = function() if self.hosting then self:requestsMenu() else self:notify(_("Start hosting first.")) end end },
        { text = _("Open host chat"), callback = function() if self.hosting then self:chatMenu() else self:notify(_("Start hosting first.")) end end },
        { text = self.discovery and _("Turn discovery off") or _("Turn discovery on"), callback = function() if self.hosting then if self:setDiscovery(not self.discovery) then self:showHostInfo() end else self:notify(_("Start hosting first.")) end end },
        { text = _("Members (%1)"):gsub("%%1", tostring(countKeys(self.users))), callback = function() if self.hosting then self:membersMenu() else self:notify(_("Start hosting first.")) end end },
        { text = _("Stop hosting and end chat"), callback = function() if self.hosting then self:stopHost() else self:notify(_("You are not hosting a chat.")) end end },
        { text = _("Back"), callback = function() self:showHome() end },
    }
    self:showMenu(_("Host controls"), rows)
end

function Chat:showHostInfo()
    local code = self.join_code or "—"
    self:notify(string.format(_("Address: %s:%d\nJoin code: %s\nHost check code: %s\nDiscovery: %s"), self:getIP(), PORT, code, self.check_code or "—", self.discovery and _("on") or _("off")))
end

function Chat:getIP()
    if not socket_ok then return "unknown" end
    local s = socket.udp()
    if not s then return "unknown" end
    s:setpeername("8.8.8.8", 80)
    local ip = s:getsockname()
    s:close()
    return ip or "unknown"
end

function Chat:startHost()
    if not socket_ok or not json_ok then
        self:log("ERROR", "Host start unavailable: required LuaSocket or JSON module missing")
        return self:notify(_("This KOReader build is missing LuaSocket or JSON support. Create a support log entry and send me the log."))
    end
    if self.hosting then return self:showHostInfo() end
    local srv, err = socket.bind("0.0.0.0", PORT)
    if not srv then
        self.status = _("Host failed: ") .. tostring(err)
        self:log("ERROR", "Host bind failed: " .. tostring(err))
        return self:notify(self.status)
    end
    srv:settimeout(0)
    self.server = srv
    self.hosting, self.running = true, true
    self.is_admin = true -- The Kindle host is the administrator; no host password.
    self.join_code = self:randomCode(8)
    self.check_code = self:randomCode(4)
    self.admin_token = self:randomCode(16)
    if not self.join_code or not self.check_code or not self.admin_token then
        pcall(function() srv:close() end)
        self.server, self.hosting, self.running = nil, false, false
        self:log("ERROR", "Cryptographic randomness unavailable; refusing to start host")
        return self:notify(_("Could not get secure random bytes from this device. Hosting was not started; send the debug log."))
    end
    self.current_host, self.auth_token = "127.0.0.1:" .. PORT, self.admin_token
    self.current_username = "Host"
    self.last_message_id = 0
    self.status = _("Hosting on port %1"):gsub("%%1", tostring(PORT))
    self:log("INFO", "Host started on TCP port " .. tostring(PORT))
    self:startPump()
    self:showHostInfo()
    self:startMessagePump()
end

function Chat:randomCode(byte_count)
    local f = io.open("/dev/urandom", "rb")
    if f then
        local count = byte_count or 4
        local b = f:read(count); f:close()
        if b and #b == count then
            local out = {}
            for i = 1, #b do out[#out + 1] = string.format("%02x", b:byte(i)) end
            return table.concat(out)
        end
    end
    return nil
end

function Chat:startPump()
    if self.pumping then return end
    self.pumping = true
    local function pump()
        if not self.hosting then self.pumping = false; return end
        local ok, err = pcall(self.pollServer, self)
        if not ok then self:log("ERROR", "Server pump failed: " .. tostring(err)) end
        UIManager:scheduleIn(0.5, pump)
    end
    UIManager:scheduleIn(0.5, pump)
end

function Chat:startMessagePump()
    if self.message_pump then return end
    local function pump()
        if not self.running then self.message_pump = nil; return end
        local ok, messages = pcall(self.pollMessages, self)
        if ok and type(messages) == "table" then
            local newest, unread = self.last_message_id or 0, 0
            for _, msg in ipairs(messages) do
                if tonumber(msg.id) and msg.id > newest then
                    if self.last_message_id and msg.from ~= self.current_username then unread = unread + 1 end
                    newest = msg.id
                end
            end
            self.last_message_id = newest
            if unread > 0 and self.chat_hidden then
                self:log("INFO", "Received " .. tostring(unread) .. " new message(s) while chat was hidden")
                self:notify(string.format(_("%d new chat message(s). Open LAN Chat to read."), unread))
            end
        elseif not ok then
            self:log("ERROR", "Background message check failed: " .. tostring(messages))
        end
        UIManager:scheduleIn(10, pump)
    end
    self.message_pump = pump
    UIManager:scheduleIn(10, pump)
end

function Chat:setDiscovery(enabled)
    if not enabled then
        self.discovery = false
        if self.discovery_server then pcall(function() self.discovery_server:close() end); self.discovery_server = nil end
        self.status = _("Discovery disabled")
        self:log("INFO", "LAN discovery disabled")
        return true
    end
    if not socket_ok then self:notify(_("LuaSocket is unavailable.")); return false end
    local udp, err = socket.udp()
    if not udp then self:log("ERROR", "Could not create discovery socket: " .. tostring(err)); self:notify(_("Could not start discovery. See debug log.")); return false end
    local bound, bind_err = udp:setsockname("*", 8766)
    if not bound then udp:close(); self:log("ERROR", "Could not bind discovery port 8766: " .. tostring(bind_err)); self:notify(_("Discovery port is unavailable. See debug log.")); return false end
    udp:settimeout(0)
    self.discovery_server = udp
    self.discovery = true
    self.status = _("Discovery enabled")
    self:log("INFO", "LAN discovery enabled on UDP port 8766")
    return true
end

function Chat:pollDiscovery()
    if not self.discovery_server then return end
    for _i = 1, 4 do
        local query, ip, port = self.discovery_server:receivefrom()
        if not query then break end
        if query == "KINDLE_LAN_CHAT_DISCOVER_V1" and self.hosting then
            self.discovery_server:sendto("KINDLE_LAN_CHAT_HERE_V1:" .. tostring(PORT), ip, port)
        end
    end
end

function Chat:scanChats()
    if not socket_ok or not json_ok then return self:notify(_("This KOReader build is missing LuaSocket or JSON support.")) end
    if self.scan_sock then pcall(function() self.scan_sock:close() end) end
    local udp, err = socket.udp()
    if not udp then self:log("ERROR", "Could not create scan socket: " .. tostring(err)); return self:notify(_("Could not scan for chats. See debug log.")) end
    pcall(function() udp:setoption("broadcast", true) end)
    local bound, bind_err = udp:setsockname("*", 0)
    if not bound then udp:close(); self:log("ERROR", "Could not bind scan socket: " .. tostring(bind_err)); return self:notify(_("Could not scan for chats. See debug log.")) end
    udp:settimeout(0)
    self.scan_sock = udp
    self.scan_results = {}
    local sent, send_err = udp:sendto("KINDLE_LAN_CHAT_DISCOVER_V1", "255.255.255.255", 8766)
    if not sent then
        udp:close(); self.scan_sock = nil
        self:log("WARN", "LAN discovery broadcast failed: " .. tostring(send_err))
        return self:notify(_("Could not send a Wi-Fi discovery broadcast. Try joining by address."))
    end
    self:log("INFO", "Started a 2-second LAN chat scan")
    local started = os.time()
    local function collect()
        if self.scan_sock ~= udp then return end
        for _i = 1, 12 do
            local answer, ip = udp:receivefrom()
            if not answer then break end
            local port = answer:match("^KINDLE_LAN_CHAT_HERE_V1:(%d+)$")
            port = tonumber(port)
            if port and ip and not self.scan_results[ip .. ":" .. port] then
                self.scan_results[ip .. ":" .. port] = { ip = ip, port = port, name = "Kindle LAN Chat" }
            end
        end
        if os.time() - started < 2 then return UIManager:scheduleIn(0.15, collect) end
        pcall(function() udp:close() end); self.scan_sock = nil
        local rows = {}
        for address, chat in pairs(self.scan_results) do
            local found = chat
            rows[#rows + 1] = { text = found.name .. " — " .. address, callback = function() self:joinMenu(found.ip, found.port) end }
        end
        if #rows == 0 then
            self:log("INFO", "LAN scan found no responding hosts")
            rows[1] = { text = _("No chats found. Hosts need discovery enabled and both devices on the same Wi-Fi." ) }
        else
            self:log("INFO", "LAN scan found " .. tostring(#rows) .. " chat host(s)")
        end
        rows[#rows + 1] = { text = _("Scan again"), callback = function() self:scanChats() end }
        rows[#rows + 1] = { text = _("Join by address"), callback = function() self:joinAddress() end }
        rows[#rows + 1] = { text = _("Back"), callback = function() self:showHome() end }
        self:showMenu(_("Nearby chats"), rows)
    end
    UIManager:scheduleIn(0.15, collect)
end

function Chat:pollServer()
    if not self.server then return end
    self:pollDiscovery()
    for _i = 1, 3 do
        local c = self.server:accept()
        if not c then break end
        if #self.clients >= MAX_CLIENTS then c:close() else
            c:settimeout(0)
            self.clients[#self.clients + 1] = { sock = c, data = "", started = os.time() }
        end
    end
    for i = #self.clients, 1, -1 do
        local client = self.clients[i]
        local chunk, err, partial = client.sock:receive(2048)
        client.data = client.data .. (chunk or partial or "")
        local dead = err == "closed" or #client.data > MAX_BODY + 4096 or os.time() - client.started > 10
        local h_end = client.data:find("\r\n\r\n", 1, true)
        if h_end then
            local len = tonumber(client.data:match("[Cc]ontent%-[Ll]ength:%s*(%d+)")) or 0
            if len > MAX_BODY then dead = true
            elseif #client.data >= h_end + 4 + len then
                local head = client.data:sub(1, h_end - 1)
                local method, path = head:match("^(%u+)%s+([^%s]+)")
                local body = client.data:sub(h_end + 4, h_end + 3 + len)
                self:handleRequest(client.sock, method, path, body, head)
                dead = true
            end
        end
        if dead then pcall(function() client.sock:close() end); table.remove(self.clients, i) end
    end
end

function Chat:respond(sock, code, obj)
    local encoded, body = pcall(json.encode, obj or {})
    if not encoded then
        self:log("ERROR", "Could not encode HTTP response JSON")
        body, code = "{}", 400
    elseif #body > MAX_RESPONSE then
        self:log("WARN", "Refused oversized HTTP response")
        body, code = "{}", 400
    end
    local status = code == 200 and "200 OK" or (code == 403 and "403 Forbidden" or (code == 404 and "404 Not Found" or "400 Bad Request"))
    sock:settimeout(1)
    local sent, err = sock:send("HTTP/1.1 " .. status .. "\r\nContent-Type: application/json\r\nConnection: close\r\nContent-Length: " .. #body .. "\r\n\r\n" .. body)
    if not sent then self:log("WARN", "HTTP response send failed: " .. tostring(err)) end
end

function Chat:handleRequest(sock, method, path, body, headers)
    if method ~= "GET" and method ~= "POST" then
        self:log("WARN", "Rejected unsupported HTTP method")
        return self:respond(sock, 400, { error = "method" })
    end
    local ok, data = pcall(json.decode, body ~= "" and body or "{}")
    if not ok or type(data) ~= "table" then
        self:log("WARN", "Rejected malformed JSON request")
        return self:respond(sock, 400, { error = "json" })
    end
    local token = headers:match("[Aa]uthorization:%s*Bearer%s+([%w%-_]+)")
    local remote
    if sock then
        local peer_ok, peer = pcall(function() return sock:getpeername() end)
        if peer_ok then remote = peer end
    end
    local throttle = remote and self.failed_auth and self.failed_auth[remote]
    if throttle and throttle.blocked_until and throttle.blocked_until > os.time() then
        self:log("WARN", "Rate-limited an authentication attempt")
        return self:respond(sock, 403, { error = "Too many attempts. Wait a minute and try again." })
    end
    local username
    if token and token == self.admin_token and self.hosting then username = "Host" end
    for name, user in pairs(self.users) do if token and user.token == token then username = name; break end end
    if path == "/join" and method == "POST" then
        local name = clean(data.username, 24)
        if name == "" or not name:match("^[%w_%-]+$") or clean(data.code, 16) ~= self.join_code then
            self:log("WARN", "Rejected account request (invalid join code or username)")
            self:noteAuthFailure(remote)
            return self:respond(sock, 403, { error = "Invalid join code or username" })
        end
        if self.users[name] then return self:respond(sock, 403, { error = "Username exists" }) end
        for _, request in ipairs(self.pending) do if request.username == name then return self:respond(sock, 200, { status = "pending" }) end end
        if #self.pending >= MAX_PENDING then return self:respond(sock, 400, { error = "Host approval queue is full" }) end
        local password = clean(data.password, 128)
        if #password < 8 then return self:respond(sock, 400, { error = "Password must be at least 8 characters" }) end
        self.pending[#self.pending + 1] = { username = name, password = password, created = os.time() }
        self:log("INFO", "Received a new account approval request")
        return self:respond(sock, 200, { status = "pending", message = "Awaiting host approval" })
    elseif path == "/login" and method == "POST" then
        local name = clean(data.username, 24)
        local user = self.users[name]
        if not user or clean(data.password, 128) ~= user.password then
            self:log("WARN", "Rejected sign-in attempt")
            self:noteAuthFailure(remote)
            return self:respond(sock, 403, { error = "Invalid credentials" })
        end
        if remote then self.failed_auth[remote] = nil end
        self:log("INFO", "Member signed in")
        return self:respond(sock, 200, { token = user.token })
    elseif path == "/messages" and method == "GET" then
        if not username then return self:respond(sock, 403, { error = "Sign in required" }) end
        local visible = {}
        for _, msg in ipairs(self.messages) do
            if msg.to == "" or msg.from == username or msg.to == username then visible[#visible + 1] = msg end
        end
        return self:respond(sock, 200, { messages = visible })
    elseif path == "/members" and method == "GET" then
        if not username then return self:respond(sock, 403, { error = "Sign in required" }) end
        local members = { "Host" }
        for name in pairs(self.users) do members[#members + 1] = name end
        table.sort(members)
        return self:respond(sock, 200, { members = members })
    elseif path == "/messages" and method == "POST" then
        if not username then return self:respond(sock, 403, { error = "Sign in required" }) end
        local text = clean(data.text, 1000)
        if text == "" then return self:respond(sock, 400, { error = "Empty message" }) end
        local recipient = clean(data.to, 24)
        if recipient ~= "" and recipient ~= "Host" and not self.users[recipient] then return self:respond(sock, 400, { error = "Unknown recipient" }) end
        local msg = { id = self.next_id, from = username, to = recipient, text = text, time = os.time() }
        self.next_id = self.next_id + 1; self.messages[#self.messages + 1] = msg
        while #self.messages > MAX_MESSAGES do table.remove(self.messages, 1) end
        return self:respond(sock, 200, { message = msg })
    elseif path == "/health" then return self:respond(sock, 200, { name = "Kindle LAN Chat", check_code = self.check_code, port = PORT }) end
    return self:respond(sock, 404, { error = "Not found" })
end

function Chat:noteAuthFailure(remote)
    if not remote then return end
    local now = os.time()
    local entry = self.failed_auth[remote]
    if not entry or now - entry.started > 60 then entry = { started = now, count = 0 }; self.failed_auth[remote] = entry end
    entry.count = entry.count + 1
    if entry.count >= 6 then entry.blocked_until = now + 60 end
end

function Chat:requestsMenu()
    local rows = {}
    for i, req in ipairs(self.pending) do
        local request_index = i
        rows[#rows + 1] = { text = req.username .. " — approve", callback = function() self:approve(request_index) end }
        rows[#rows + 1] = { text = req.username .. " — reject", callback = function() table.remove(self.pending, request_index); self:requestsMenu() end }
    end
    if #rows == 0 then rows[1] = { text = _("No pending requests") } end
    rows[#rows + 1] = { text = _("Back"), callback = function() self:hostMenu() end }
    self:showMenu(_("Join requests"), rows)
end

function Chat:approve(i)
    local req = self.pending[i]; if not req then return end
    self.users[req.username] = { password = req.password, token = self:randomCode(16) }
    table.remove(self.pending, i); self:save(); self:log("INFO", "Host approved an account request"); self:notify(_("Account approved for ") .. req.username); self:requestsMenu()
end

function Chat:membersMenu()
    local rows = {}
    for name in pairs(self.users) do rows[#rows + 1] = { text = name } end
    if #rows == 0 then rows[1] = { text = _("No members yet") } end
    rows[#rows + 1] = { text = _("Back"), callback = function() self:hostMenu() end }
    self:showMenu(_("Members"), rows)
end

function Chat:stopHost()
    UIManager:show(ConfirmBox:new{ text = _("Stop hosting? This ends the chat service."), ok_text = _("Stop"),
        ok_callback = function()
            self.hosting, self.running = false, false; self.status = _("Stopped")
            self:log("INFO", "Host stopped by user; active session data cleared")
            self:setDiscovery(false)
            if self.server then pcall(function() self.server:close() end); self.server = nil end
            for _, c in ipairs(self.clients) do pcall(function() c.sock:close() end) end
            self.clients, self.messages, self.users, self.pending = {}, {}, {}, {}
            self.admin_token, self.auth_token, self.current_host = nil, nil, nil
            self.settings.data.users = {}; self:save(); self.join_code, self.check_code = nil, nil
            self:notify(_("Chat stopped; member accounts and messages were cleared.")); self:showHome()
        end })
end

function Chat:joinAddress()
    self:input(_("Host address (IPv4:port)"), "", function(value)
        local ip, port = clean(value, 64):match("^([%d%.]+):(%d+)$")
        port = tonumber(port)
        if not ip or not port or port < 1 or port > 65535 or not ip:match("^%d+%.%d+%.%d+%.%d+$") then return self:notify(_("Enter an IPv4 address and port, such as 192.168.1.5:8765.")) end
        self.current_host = ip .. ":" .. port
        self.settings.data.recent[self.current_host] = os.time(); self.settings:flush()
        self:joinMenu(ip, port)
    end)
end

function Chat:joinMenu(ip, port)
    self:showMenu(ip .. ":" .. port, {
        { text = _("Request account"), callback = function() self:verifyHost(ip, port, function() self:requestAccount(ip, port) end) end },
        { text = _("Sign in"), callback = function() self:verifyHost(ip, port, function() self:login(ip, port) end) end },
        { text = _("Back"), callback = function() self:showHome() end },
    })
end

function Chat:verifyHost(ip, port, callback)
    local info = self:http(ip, port, "GET", "/health", {})
    if not info or not info.check_code then
        self:log("WARN", "Host verification request failed")
        self:notify(_("Could not verify this host. Check the address and Wi-Fi, then try again."))
        return
    end
    local prompt = string.format(_("Host: %s — compare check code %s with the host Kindle"), info.name or "Kindle LAN Chat", info.check_code)
    self:input(prompt, "", function(answer)
        if clean(answer, 16):lower() ~= tostring(info.check_code):lower() then
            self:log("WARN", "Host check code did not match")
            self:notify(_("Check code did not match. Nothing was sent to the host."))
            return
        end
        self:log("INFO", "Host check code matched")
        callback()
    end)
end

function Chat:requestAccount(ip, port)
    self:input(_("Username"), "", function(name)
        self:input(_("Password (at least 8 characters)"), "", function(pass)
            self:input(_("Join code"), "", function(code)
                local response = self:http(ip, port, "POST", "/join", { username = clean(name, 24), password = clean(pass, 128), code = clean(code, 16) })
                self:notify(response and (response.message or response.status or response.error) or _("Request failed."))
                self:joinMenu(ip, port)
            end)
        end)
    end)
end

function Chat:login(ip, port)
    self:input(_("Username"), "", function(name)
        self:input(_("Password"), "", function(pass)
            local response = self:http(ip, port, "POST", "/login", { username = clean(name, 24), password = clean(pass, 128) })
            if response and response.token then
                self.current_host, self.auth_token = ip .. ":" .. port, response.token
                self.current_username = clean(name, 24)
                self.last_message_id = nil
                self.running, self.chat_hidden = true, true
                self.status = _("Connected as ") .. self.current_username
                self:startMessagePump()
                self:chatMenu()
            else self:notify(response and response.error or _("Could not connect. Check address, Wi-Fi, and host status.")) end
        end)
    end)
end

function Chat:http(ip, port, method, path, data)
    if not socket_ok or not json_ok then self:log("ERROR", "Network request unavailable: LuaSocket or JSON module missing"); return nil end
    local s = socket.tcp(); s:settimeout(3)
    local ok, connect_err = s:connect(ip, port)
    if not ok then s:close(); self:log("WARN", "Connection failed for " .. tostring(ip) .. ":" .. tostring(port) .. " (" .. tostring(connect_err) .. ")"); return nil end
    local body = json.encode(data or {})
    local auth = self.auth_token and ("Authorization: Bearer " .. self.auth_token .. "\r\n") or ""
    local wire_body = method == "POST" and body or ""
    local sent, send_err = s:send(method .. " " .. path .. " HTTP/1.1\r\nHost: " .. ip .. "\r\n" .. auth .. "Content-Type: application/json\r\nContent-Length: " .. #wire_body .. "\r\nConnection: close\r\n\r\n" .. wire_body)
    if not sent then s:close(); self:log("WARN", "Request send failed: " .. tostring(send_err)); return nil end
    local response, err, partial = s:receive("*a"); response = response or partial or ""; s:close()
    local payload = response:match("\r\n\r\n(.*)")
    if not payload or #payload > MAX_RESPONSE then self:log("WARN", "Server returned an invalid or oversized response"); return nil end
    local dec, result = pcall(json.decode, payload)
    if not dec then self:log("WARN", "Server response contained malformed JSON") end
    return dec and result or nil
end

function Chat:chatMenu()
    self.chat_hidden = false
    local response = self.http and self.current_host and self.auth_token and self:pollMessages() or nil
    local rows = {}
    for _, msg in ipairs(response or {}) do
        self.last_message_id = math.max(self.last_message_id or 0, tonumber(msg.id) or 0)
        local target = msg.to ~= "" and (" → " .. msg.to) or ""
        rows[#rows + 1] = { text = msg.from .. target .. ": " .. msg.text }
    end
    rows[#rows + 1] = { text = _("Send message"), callback = function() self:sendMessage() end }
    rows[#rows + 1] = { text = self.selected_recipient and (_("Recipient: ") .. self.selected_recipient) or _("Choose recipient"), callback = function() self:chooseRecipient() end }
    rows[#rows + 1] = { text = _("Refresh"), callback = function() self:chatMenu() end }
    rows[#rows + 1] = { text = _("Hide chat (keep connected)"), callback = function() self:hideChat() end }
    rows[#rows + 1] = { text = self.hosting and _("Hide chat (hosting continues)") or _("Disconnect"), callback = function()
        if self.hosting then self:hideChat()
        else
            self.running, self.auth_token, self.current_username = false, nil, nil
            self.status = _("Disconnected")
            self:log("INFO", "Disconnected from chat")
            self:showHome()
        end
    end }
    self:showMenu(_("LAN Chat"), rows)
end

function Chat:hideChat()
    self.chat_hidden = true
    self.status = self.hosting and _("Hosting; chat hidden") or _("Connected; chat hidden")
    self:log("INFO", "Chat interface hidden; connection polling remains active")
    self:showHome()
end

function Chat:chooseRecipient()
    local ip, port = (self.current_host or ""):match("^([%d%.]+):(%d+)$")
    if not ip then return self:notify(_("Not connected to a host.")) end
    local data = self:http(ip, tonumber(port), "GET", "/members", {})
    if not data or type(data.members) ~= "table" then return self:notify(_("Could not load members. Refresh the connection and try again.")) end
    local rows = {
        { text = _("Everyone"), callback = function() self.selected_recipient = nil; self:chatMenu() end },
    }
    for _, name in ipairs(data.members) do
        local member_name = name
        rows[#rows + 1] = { text = member_name, callback = function() self.selected_recipient = member_name; self:chatMenu() end }
    end
    rows[#rows + 1] = { text = _("Back"), callback = function() self:chatMenu() end }
    self:showMenu(_("Choose recipient"), rows)
end

function Chat:pollMessages()
    local ip, port = (self.current_host or ""):match("^([%d%.]+):(%d+)$"); if not ip then return {} end
    local r = self:http(ip, tonumber(port), "GET", "/messages", {})
    if r and r.messages then return r.messages end
    self.status = _("Connection lost; retry from the chat menu")
    return {}
end

function Chat:sendMessage()
    self:input(_("Message (up to 1000 characters)"), "", function(text)
        local ip, port = (self.current_host or ""):match("^([%d%.]+):(%d+)$")
        if ip then
            local response = self:http(ip, tonumber(port), "POST", "/messages", { text = clean(text, 1000), to = self.selected_recipient or "" })
            if not response or response.error then self:log("WARN", "Message send failed"); self:notify(_("Message was not sent. Check the connection and debug log."))
            else self:log("INFO", "Message sent") end
        end
        self:chatMenu()
    end)
end

function Chat:recentMenu()
    local rows = {}
    for address in pairs(self.settings.data.recent) do
        local recent_address = address
        rows[#rows + 1] = { text = recent_address, callback = function() self.current_host = recent_address; local ip, port = recent_address:match("^([%d%.]+):(%d+)$"); self:joinMenu(ip, tonumber(port)) end }
    end
    if #rows == 0 then rows[1] = { text = _("No recent hosts") } end
    rows[#rows + 1] = { text = _("Back"), callback = function() self:showHome() end }
    self:showMenu(_("Recent hosts"), rows)
end

return Chat
