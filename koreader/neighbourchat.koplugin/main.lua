local Device = require("device")
if not Device:isKindle() then return {disabled=true} end

local WidgetContainer = require("ui/widget/container/widgetcontainer")
local UIManager = require("ui/uimanager")
local Menu = require("ui/widget/menu")
local InputDialog = require("ui/widget/inputdialog")
local MultiInputDialog = require("ui/widget/multiinputdialog")
local ConfirmBox = require("ui/widget/confirmbox")
local InfoMessage = require("ui/widget/infomessage")
local TextViewer = require("ui/widget/textviewer")
local Dispatcher = require("dispatcher")

local source = debug.getinfo(1, "S").source
local path = source:match("^@(.+)/[^/]+$") or "plugins/neighbourchat.koplugin"
local client_ok, Client = pcall(dofile, path .. "/client.lua")
local bootstrap_error
if not client_ok or type(Client) ~= "table" then
    bootstrap_error = "Neighbour Chat client.lua could not be loaded safely: " .. tostring(Client)
end
if not Client then
    Client = {}
    function Client.new()
        return {
            state={stage="error", error=bootstrap_error or "Neighbour Chat is unavailable."},
            start=function() return nil, bootstrap_error end,
            readState=function() return nil, bootstrap_error end,
            action=function() return nil, bootstrap_error end,
            stop=function() return nil, bootstrap_error end,
        }
    end
end
local shared = package.loaded["neighbourchat.session"]
if not shared then
    shared = {client=Client.new(path), draft="", full=false, background=nil}
    package.loaded["neighbourchat.session"] = shared
end

local client = shared.client
local Chat = WidgetContainer:extend{name="neighbourchat", is_doc_only=false}

local function safe_call(fn, ...)
    local ok, result = pcall(fn, ...)
    if ok then return true, result end
    return false, tostring(result)
end

local function row(text, callback, enabled)
    return {text=text, callback=callback, enabled=enabled ~= false}
end
local function list(value)
    return type(value) == "table" and value or {}
end

function Chat:init()
    shared.background = self
    self.ui.menu:registerToMainMenu(self)
    Dispatcher:registerAction("neighbourchat_popup", {
        category="none", event="NeighbourChatPopup",
        title="Neighbour Chat: popup", general=true,
    })
    Dispatcher:registerAction("neighbourchat_full", {
        category="none", event="NeighbourChatFull",
        title="Neighbour Chat: full screen", general=true,
    })
end

function Chat:addToMainMenu(items)
    items.neighbourchat = {
        text="Neighbour Chat", sorting_hint="more_tools", sub_item_table={
            {text="Open chat", callback=function() self:open(false) end},
            {text="Host on this Kindle", callback=function() self:startServer() end},
            {text="Find nearby chats", callback=function() self:open(false) end},
            {text="Full-screen chat", callback=function() self:open(true) end},
            {text="Disconnect", callback=function() self:disconnect() end},
            {text="Stop chat service", callback=function() self:stop() end},
        },
    }
end

function Chat:startServer()
    shared.pending_action = {action="host"}
    self:open(false)
end

function Chat:connectToServer()
    shared.pending_screen = "connect"
    self:open(false)
end

function Chat:onNeighbourChatPopup()
    self:open(false)
    return true
end

function Chat:onNeighbourChatFull()
    self:open(true)
    return true
end

function Chat:message(text)
    safe_call(function()
        UIManager:show(InfoMessage:new{text=tostring(text or "Chat error"), timeout=5})
    end)
end

function Chat:notify(text)
    local now = os.time()
    if shared.last_notice_at and now - shared.last_notice_at < 10 then return end
    shared.last_notice_at = now
    safe_call(function()
        UIManager:show(InfoMessage:new{text=tostring(text or "New chat message"), timeout=3})
    end)
end

function Chat:closeMenu()
    if self.menu then
        local menu = self.menu
        self.menu = nil
        self.closing_menu = true
        safe_call(function() UIManager:close(menu) end)
        self.closing_menu = nil
    end
end

function Chat:onMenuClosed(menu)
    if self.menu == menu then self.menu = nil end
    if not self.closing_menu and shared.owner == self then
        self:hide()
    end
end

function Chat:cancelBackground()
    if shared.background_poll then
        safe_call(function() UIManager:unschedule(shared.background_poll) end)
        shared.background_poll = nil
    end
end

function Chat:hide(keep_background)
    self.generation = (self.generation or 0) + 1
    if shared.owner == self then shared.owner = nil end
    self:closeMenu()
    if self.dialog then
        local dialog = self.dialog
        self.dialog = nil
        safe_call(function() UIManager:close(dialog) end)
    end
    if self.viewer then
        local viewer = self.viewer
        self.viewer = nil
        safe_call(function() UIManager:close(viewer) end)
    end
    if keep_background == false then
        self:cancelBackground()
        shared.background = nil
    else
        shared.background = self
        self:scheduleBackground(5)
    end
end

function Chat:open(full)
    if shared.owner and shared.owner ~= self then shared.owner:hide() end
    self:hide()
    self:cancelBackground()
    shared.owner = self
    shared.background = self
    self.generation = (self.generation or 0) + 1
    shared.full = full == true
    shared.auto_scan = not shared.pending_action and not shared.pending_screen
    self.failed_reads = 0
    self:render()
    self:scheduleBackground(0.1)
end

function Chat:scheduleBackground(delay)
    if shared.background ~= self or shared.background_poll then return end
    local callback
    callback = function()
        shared.background_poll = nil
        self:tick()
    end
    shared.background_poll = callback
    safe_call(function() UIManager:scheduleIn(delay, callback) end)
end

function Chat:tick()
    if shared.background ~= self then return end
    local ok, err = true, nil
    if not shared.ready then
        ok, err = safe_call(function()
            local ready, start_error = client:start()
            if ready then
                shared.ready = true
                client.starting = nil
            elseif ready == nil then
                client.state = {stage="idle", error=start_error}
            else
                client.state = {stage="starting"}
            end
        end)
    end
    if ok and shared.ready then
        ok, err = safe_call(function()
            local state, read_error = client:readState()
            if state then
                self.failed_reads = 0
                if not state.busy and shared.pending_action then
                    local action = shared.pending_action
                    shared.pending_action = nil
                    self:action(action)
                elseif not state.busy and shared.pending_screen == "connect" then
                    shared.pending_screen = nil
                    self:join()
                elseif not state.busy and shared.auto_scan and state.stage == "idle" and not state.connected then
                    shared.auto_scan = nil
                    self:action{action="scan"}
                end
                if not state.busy and shared.show_ip_pending then
                    shared.show_ip_pending = nil
                    self:displayHostIP(state)
                end
                if shared.teardown and not state.busy then
                    if state.error and state.error ~= "" then
                        shared.teardown = nil
                        self:message("Chat shutdown failed: " .. tostring(state.error))
                    else
                        shared.teardown = nil
                        shared.draft = ""
                        shared.recipient = nil
                        client.state = {stage="idle", notice=state.notice}
                    end
                end
                local newest, author
                for _, message in ipairs(list(state.messages)) do
                    if type(message) == "table" then
                        local id = tonumber(message.id or 0) or 0
                        if not newest or id > newest then
                            newest = id
                            author = message.display_name
                        end
                    end
                end
                if newest and shared.last_message_id and newest > shared.last_message_id and not shared.owner then
                    self:notify("New message" .. (author and (" from " .. tostring(author)) or ""))
                end
                if newest and (not shared.last_message_id or newest > shared.last_message_id) then
                    shared.last_message_id = newest
                end
            else
                self.failed_reads = (self.failed_reads or 0) + 1
                client.state.error = read_error
                if self.failed_reads >= 3 then
                    shared.ready = nil
                    client.token = nil
                    client.starting = nil
                    client.state = {stage="idle", error=read_error}
                end
            end
        end)
    end
    if not ok then
        shared.ready = nil
        client.token = nil
        client.starting = nil
        client.state = {stage="idle", error=err}
    end
    if shared.owner == self then self:render() end
    self:scheduleBackground(shared.ready and 5 or 2)
end

function Chat:render()
    if shared.owner ~= self or self.dialog then return end
    self:closeMenu()
    local s = client.state or {}
    local title = "Neighbour Chat"
    local rows = {}
    local generation = self.generation
    local function add(text, callback, enabled)
        rows[#rows + 1] = row(text, function()
            self:closeMenu()
            if shared.owner == self and self.generation == generation then safe_call(callback) end
        end, enabled)
    end
    if s.error and s.error ~= "" then
        title = shared.ready and "Could not complete that action" or "Chat service unavailable"
        add("Details: " .. tostring(s.error), function() end, false)
        if shared.ready then
            add("Try scan again", function() self:action{action="scan"} end)
            add("Join by Wi-Fi address", function() self:join() end)
            add("Host on this Kindle", function() self:action{action="host"} end)
            add("Home", function() self:home() end)
        else
            add("Start / retry service", function() self:startService() end)
            add("Retry", function() client.starting=nil; shared.ready=nil; self:render() end)
        end
        add("Close", function() self:hide() end)
    elseif not shared.ready or s.stage == "starting" then
        title = "Starting chat"
        add("Please wait…", function() end, false)
        add("Hide", function() self:hide() end)
    elseif s.stage == "verify" then
        title = "Verify host"
        add("Codes match — continue", function() self:action{action="confirm", token=s.verify_token} end)
        add("Choose another host", function() self:home() end)
    elseif s.stage == "login" then
        title = "Sign in"
        add("Sign in", function() self:login() end)
        add("Request an account", function() self:register() end)
        add("Choose another host", function() self:home() end)
    elseif s.stage == "locked" or s.vault_locked then
        title = "Saved content locked"
        add("Unlock saved content", function() self:unlock() end)
        add("Hide", function() self:hide() end)
    elseif s.busy then
        title = shared.teardown and "Ending chat…" or "Working…"
        add(shared.teardown and "Waiting for safe shutdown" or "Please wait…", function() end, false)
        add("Hide", function() self:hide() end)
    elseif s.connected then
        title = (s.address == "This Kindle" and "HOST / ADMIN" or "Neighbour Chat")
        add("Connected" .. (s.display and (" as " .. tostring(s.display)) or ""), function() end, false)
        add("Open messages", function() self:messages() end)
        add("People", function() self:people() end)
        if s.admin then
            add("Show my IP address", function()
                shared.show_ip_pending = true
                self:action{action="invite"}
            end)
            add("Host tools", function() self:hostTools() end)
        end
        add("Disconnect", function() self:disconnect() end)
        add("End chat", function() self:endChat() end)
        add("Hide", function() self:hide() end)
    else
        title = s.hosting and "YOUR KINDLE | HOST" or "NEIGHBOUR CHAT | HOME"
        add(s.hosting and "Open your hosted chat" or "Host on this Kindle", function() self:action{action="host"} end)
        add("Scan nearby chats - refresh", function() shared.auto_scan=nil; self:action{action="scan"} end)
        add("Join by Wi-Fi address", function() self:join() end)
        if s.hosting then
            add("Show my IP address", function()
                shared.show_ip_pending = true
                self:action{action="invite"}
            end)
            add("Host controls", function() self:hostTools() end)
        end
        local nearby = list(s.nearby)
        if #nearby > 0 then
            add("NEARBY CHATS", function() end, false)
            for _, host in ipairs(nearby) do
                if type(host) == "table" then
                    local name = tostring(host.name or "Nearby chat")
                    local address = tostring(host.address or "")
                    add(name .. (address ~= "" and ("\n" .. address) or ""), function()
                        self:action{action="join-nearby", address=address}
                    end)
                end
            end
        elseif s.scanned then
            add("No chats found. Ask the host to turn discovery on, or join by address.", function() end, false)
        else
            add("Searching your Wi-Fi for chats...", function() end, false)
        end
        local recent = list(s.recent_hosts)
        if #recent > 0 then
            add("RECENT CHATS", function() end, false)
            for _, address in ipairs(recent) do
                local saved_address = tostring(address)
                add("Reconnect - " .. saved_address, function()
                    self:action{action="reconnect", address=saved_address}
                end)
            end
        end
        add("Close", function() self:hide() end)
    end

    local screen_width = Device.screen:getWidth()
    local screen_height = Device.screen:getHeight()
    local full = shared.full == true
    local menu
    local ok, value = safe_call(function()
        menu = Menu:new{
            title=title,
            subtitle=(s.notice and s.notice ~= "") and s.notice or nil,
            item_table=rows,
            width=full and math.floor(screen_width * 0.98) or math.floor(screen_width * 0.86),
            height=full and math.floor(screen_height * 0.96) or math.floor(screen_height * 0.72),
            is_popout=not full,
            items_max_lines=2,
            close_callback=function() self:onMenuClosed(menu) end,
        }
        return menu
    end)
    if not ok then menu = nil end
    if not ok or not menu then
        self:message(ok and "Could not draw the chat screen safely." or value)
        return
    end
    self.menu = menu
    safe_call(function() UIManager:show(menu) end)
end

function Chat:startService()
    shared.background = self
    shared.ready = nil
    client.starting = nil
    self:open(shared.full == true)
end

function Chat:disconnect()
    if not shared.ready then
        self:message("Chat service is not running.")
        return
    end
    self:confirm("Disconnect from this chat? The service and saved login state remain available.", "Disconnect", function()
        if self:action{action="disconnect"} then
            shared.draft = ""
            shared.recipient = nil
            self:render()
        end
    end)
end

function Chat:home()
    client.state = {stage="idle", hosting=client.state.hosting, nearby={}, scanned=false}
    shared.auto_scan = true
    self:render()
end

function Chat:displayHostIP(state)
    local lines = {"Share one of these addresses with someone on the same Wi-Fi:", ""}
    for _, address in ipairs(list(state.addresses)) do
        lines[#lines + 1] = tostring(address)
    end
    if #lines == 2 then
        lines[#lines + 1] = "No Wi-Fi address is available. Connect this Kindle to Wi-Fi and try again."
    end
    if state.join_code and state.join_code ~= "" then
        lines[#lines + 1] = ""
        lines[#lines + 1] = "Join code: " .. tostring(state.join_code)
        lines[#lines + 1] = "Turn discovery on in Host controls for automatic finding."
    end
    self:showTextViewer("Your chat - IP address", table.concat(lines, "\n"))
end

function Chat:showTextViewer(title, text)
    self:closeMenu()
    local viewer
    local ok, value = safe_call(function()
        viewer = TextViewer:new{title=title, text=text}
        return viewer
    end)
    if not ok or not viewer then
        self:message(ok and "Could not open this screen." or value)
        return
    end
    self.viewer = viewer
    viewer.onCloseWidget = function()
        if self.viewer == viewer then self.viewer = nil end
        if shared.owner == self then self:render() end
    end
    safe_call(function() UIManager:show(viewer) end)
end

function Chat:action(value)
    if not shared.ready then
        self:message("Chat service is not ready.")
        return false
    end
    local ok, result = safe_call(function() return client:action(value) end)
    if not ok or not result then
        self:message(ok and "Chat operation failed." or result)
        return false
    end
    client.state.busy = true
    client.state.error = ""
    self:render()
    return true
end

function Chat:fields(title, fields, done, submit)
    if self.dialog then return end
    self.dialog = true
    local generation = self.generation
    local dialog
    local function wipe()
        if dialog and dialog.fields then
            for _, field in ipairs(dialog.fields) do
                if field and field.text then field.text = "" end
            end
        end
    end
    local function close()
        if dialog then safe_call(function() UIManager:close(dialog) end) end
        wipe()
        self.dialog = nil
    end
    local ok, value = safe_call(function()
        return MultiInputDialog:new{title=title, fields=fields, buttons={
            {text="Cancel", callback=close},
            {text=submit or "Continue", callback=function()
                local values = dialog:getFields()
                close()
                if shared.owner == self and self.generation == generation then safe_call(done, values) end
            end},
        }}
    end)
    if not ok then
        self.dialog = nil
        self:message(value)
        return
    end
    dialog = value
    dialog.onCloseWidget = function()
        wipe()
        self.dialog = nil
    end
    safe_call(function() UIManager:show(dialog); dialog:onShowKeyboard() end)
end

function Chat:join()
    self:fields("Host Wi-Fi address", {{text=shared.last_address or "", hint="192.168.1.42:8000"}}, function(values)
        shared.last_address = values[1]
        self:action{action="probe", address=values[1]}
    end, "Find host")
end

function Chat:login()
    local s = client.state or {}
    self:fields("Sign in", {
        {description="Username", text=shared.username or ""},
        {description="Password", text="", text_type="password"},
    }, function(values)
        shared.username = values[1]
        self:action{action="login", username=values[1], password=values[2]}
    end, "Sign in")
end

function Chat:unlock()
    self:fields("Unlock saved content", {
        {description="Encryption password", text="", text_type="password"},
    }, function(values) self:action{action="unlock", password=values[1]} end, "Unlock")
end

function Chat:register()
    self:fields("Request an account", {
        {description="Host join code", text=""},
        {description="Display name", text=""},
        {description="Username", text=""},
        {description="Password (10+ characters)", text="", text_type="password"},
    }, function(values)
        self:action{action="register", join_code=values[1], display=values[2], username=values[3], password=values[4]}
    end, "Request account")
end

function Chat:messages()
    local s = client.state or {}
    local text = {}
    for _, message in ipairs(list(s.messages)) do
        if type(message) == "table" then
            text[#text + 1] = tostring(message.display_name or "Someone") .. ":"
            text[#text + 1] = tostring(message.body or "")
        end
        text[#text + 1] = ""
    end
    local body = #text > 0 and table.concat(text, "\n") or "No messages yet."
    self:showTextViewer("Messages", body)
end

function Chat:people()
    local rows = {}
    for _, user in ipairs(list((client.state or {}).users)) do
        if type(user) == "table" then
            rows[#rows + 1] = row(tostring(user.display_name or "Unnamed"), function()
                shared.recipient = {id=tostring(user.id or ""), label=tostring(user.display_name or "Person")}
                self:write()
            end)
        end
    end
    if #rows == 0 then rows[1] = row("No approved people yet.", function() end, false) end
    self:showRows("People", rows)
end

function Chat:hostTools()
    local s = client.state or {}
    local rows = {
        row("Show my IP address", function()
            shared.show_ip_pending = true
            self:action{action="invite"}
        end),
        row("Invite / check code", function() self:action{action="invite"} end),
        row("Refresh host data", function() self:action{action="admin"} end),
        row((s.discoverable and "Discovery: ON (tap to turn off)" or "Discovery: OFF (tap to turn on)"), function()
            self:action{action=s.discoverable and "discovery-off" or "discovery-on", name="Neighbour Chat"}
        end),
        row("End chat and stop hosting", function() self:endChat() end),
        row("Hide", function() self:hide() end),
    }
    if type(s.pending) == "table" and #s.pending > 0 then
        rows[#rows + 1] = row("Approve: " .. tostring(s.pending[1].display or s.pending[1].username or "request"), function() self:action{action="approve", id=s.pending[1].id} end)
        rows[#rows + 1] = row("Reject: " .. tostring(s.pending[1].display or s.pending[1].username or "request"), function() self:action{action="deny", id=s.pending[1].id} end)
    end
    self:showRows("Host tools", rows)
end

function Chat:showRows(title, rows)
    self:closeMenu()
    local screen_width = Device.screen:getWidth()
    local screen_height = Device.screen:getHeight()
    local full = shared.full == true
    local menu
    local ok, value = safe_call(function()
        menu = Menu:new{
            title=title,
            item_table=rows,
            width=full and math.floor(screen_width * 0.98) or math.floor(screen_width * 0.86),
            height=full and math.floor(screen_height * 0.96) or math.floor(screen_height * 0.72),
            is_popout=not full,
            items_max_lines=2,
            close_callback=function() self:onMenuClosed(menu) end,
        }
        return menu
    end)
    if not ok then menu = nil end
    if ok and menu then
        self.menu = menu
        safe_call(function() UIManager:show(menu) end)
    else
        self:message(ok and "Could not draw this screen safely." or value)
    end
end

function Chat:write()
    if self.dialog then return end
    local recipient = shared.recipient or {label="everyone", id=""}
    self.dialog = true
    local generation = self.generation
    local dialog
    local function close()
        if dialog then safe_call(function() UIManager:close(dialog) end) end
        self.dialog = nil
    end
    local ok, value = safe_call(function()
        return InputDialog:new{
            title="Message to " .. tostring(recipient.label),
            input=shared.draft or "",
            input_hint="Write a message",
            allow_newline=true,
            buttons={
                {text="Cancel", callback=close},
                {text="Send", callback=function()
                    local body = tostring(dialog:getInputText() or "")
                    if body == "" then return end
                    if shared.owner ~= self or self.generation ~= generation then return end
                    shared.draft = body
                    close()
                    self:action{
                        action="send", body=body, recipient=recipient.id,
                        token="reader-" .. tostring(os.time()) .. "-" .. tostring(math.random(100000, 999999)),
                        context=client.state.context,
                    }
                end},
            },
        }
    end)
    if not ok then
        self.dialog = nil
        self:message(value)
        return
    end
    dialog = value
    dialog.onCloseWidget = function() self.dialog = nil end
    safe_call(function() UIManager:show(dialog); dialog:onShowKeyboard() end)
end

function Chat:endChat()
    self:confirm("End this chat? The host stops hosting. Saved chats and backups remain.", "End chat", function()
        if self:action{action="end-chat"} then
            shared.teardown = true
            self:render()
        end
    end)
end

function Chat:confirm(text, label, callback)
    if self.dialog then return end
    self.dialog = true
    local generation = self.generation
    safe_call(function()
        local dialog
        dialog = ConfirmBox:new{
            text=text, ok_text=label, ok_callback=function()
                if shared.owner == self and self.generation == generation then safe_call(callback) end
                self.dialog = nil
            end,
        }
        dialog.onCloseWidget = function() self.dialog = nil end
        self.dialog = dialog
        UIManager:show(dialog)
    end)
end

function Chat:stop()
    self:confirm("Stop the background chat service? Saved data remains on the Kindle.", "Stop service", function()
        local ok, result, err = pcall(function() return client:stop() end)
        if not ok or not result then
            self:message(ok and tostring(err or "The service did not stop.") or tostring(result))
            return
        end
        client.state.busy = true
        local started = os.time()
        local function wait_for_stop()
            local call_ok, healthy, health_error = pcall(function() return client:health() end)
            if not call_ok or not healthy then
                shared.ready = nil
                client.state = {stage="idle"}
                self:hide(false)
                return
            end
            if os.time() - started >= 6 then
                client.state.busy = false
                client.state.error = tostring(health_error or "Timed out waiting for the chat service to stop.")
                self:message(client.state.error)
                if shared.owner == self then self:render() end
                return
            end
            safe_call(function() UIManager:scheduleIn(0.3, wait_for_stop) end)
        end
        safe_call(function() UIManager:scheduleIn(0.2, wait_for_stop) end)
    end)
end

function Chat:stopImmediately()
    safe_call(function() client:stop() end)
    self:hide(false)
end

function Chat:onSuspend()
    if shared.ready then safe_call(function() client:action{action="pause"} end) end
end

function Chat:onResume()
    if shared.ready then safe_call(function() client:action{action="resume"} end) end
end

function Chat:onCloseDocument()
    self:hide()
end

function Chat:onExit()
    self:stopImmediately()
end

return Chat
