local Device = require("device")
if not Device:isKindle() then return {disabled=true} end

local WidgetContainer = require("ui/widget/container/widgetcontainer")
local UIManager = require("ui/uimanager")
local NetworkMgr = require("ui/network/manager")
local Menu = require("ui/widget/menu")
local InputDialog = require("ui/widget/inputdialog")
local MultiInputDialog = require("ui/widget/multiinputdialog")
local ConfirmBox = require("ui/widget/confirmbox")
local InfoMessage = require("ui/widget/infomessage")
local TextViewer = require("ui/widget/textviewer")
local Dispatcher = require("dispatcher")

local source = debug.getinfo(1, "S").source
local path = source:match("^@(.+)/[^/]+$") or "plugins/neighbourchat.koplugin"
local Client = dofile(path .. "/client.lua")
local shared = package.loaded["neighbourchat.session"]
if not shared then
    shared = {client=Client.new(path), draft="", full=false}
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
            {text="Open full screen", callback=function() self:open(true) end},
            {text="End chat", callback=function() self:endChat() end},
            {text="Stop chat service", callback=function() self:stop() end},
        },
    }
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

function Chat:closeMenu()
    if self.menu then
        local menu = self.menu
        self.menu = nil
        safe_call(function() UIManager:close(menu) end)
    end
end

function Chat:hide()
    self.generation = (self.generation or 0) + 1
    if self.poll then
        safe_call(function() UIManager:unschedule(self.poll) end)
        self.poll = nil
    end
    self:closeMenu()
    if self.dialog then
        local dialog = self.dialog
        self.dialog = nil
        safe_call(function() UIManager:close(dialog) end)
    end
    if shared.owner == self then shared.owner = nil end
end

function Chat:open(full)
    if shared.owner and shared.owner ~= self then shared.owner:hide() end
    self:hide()
    shared.owner = self
    self.generation = (self.generation or 0) + 1
    shared.full = full == true
    self.failed_reads = 0
    self:render()
    self.poll = function() self:tick() end
    safe_call(function() UIManager:scheduleIn(0.1, self.poll) end)
end

function Chat:tick()
    if shared.owner ~= self then return end
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
    if shared.owner == self and not self.dialog then
        safe_call(function() UIManager:scheduleIn(shared.ready and 2 or 1, self.poll) end)
    end
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
        title = "Chat error"
        add("Retry", function() client.starting=nil; shared.ready=nil; self:render() end)
        add("Hide", function() self:hide() end)
    elseif not shared.ready or s.stage == "starting" then
        title = "Starting chat"
        add("Please wait…", function() end, false)
        add("Hide", function() self:hide() end)
    elseif s.stage == "setup" then
        title = "Set up host"
        add("Choose host password", function() self:setup() end)
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
    elseif s.connected then
        title = (s.address == "This Kindle" and "HOST / ADMIN" or "Neighbour Chat")
        add("Open messages", function() self:messages() end)
        add("People", function() self:people() end)
        if s.admin then add("Host tools", function() self:hostTools() end) end
        add("End chat", function() self:endChat() end)
        add("Hide", function() self:hide() end)
    else
        title = "Start or join a chat"
        add("Host on this Kindle", function() self:action{action="host"} end)
        add("Join by Wi-Fi address", function() self:join() end)
        add("Scan nearby chats", function() self:action{action="scan"} end)
        add("Hide", function() self:hide() end)
    end
    local ok, menu = safe_call(function()
        return Menu:new{title=title, item_table=rows, width=Device.screen:getWidth()}
    end)
    if not ok or not menu then
        self:message("Could not draw the chat screen safely.")
        return
    end
    self.menu = menu
    safe_call(function() UIManager:show(menu) end)
end

function Chat:home()
    client.state = {stage="idle", hosting=client.state.hosting}
    self:render()
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
    local function close()
        if dialog then safe_call(function() UIManager:close(dialog) end) end
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
        {description="Username", text=s.address == "This Kindle" and "admin" or (shared.username or "")},
        {description="Password", text="", text_type="password"},
    }, function(values)
        shared.username = values[1]
        self:action{action="login", username=values[1], password=values[2]}
    end, "Sign in")
end

function Chat:setup()
    self:fields("Choose host password", {
        {description="Password (10+ characters)", text="", text_type="password"},
        {description="Repeat password", text="", text_type="password"},
    }, function(values)
        self:action{action="setup", password=values[1], confirm=values[2]}
    end, "Start hosting")
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
    safe_call(function() UIManager:show(TextViewer:new{title="Messages", text=body}) end)
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
        row("Invite / check code", function() self:action{action="invite"} end),
        row("Refresh host data", function() self:action{action="admin"} end),
        row("End chat and stop hosting", function() self:endChat() end),
        row("Hide", function() self:hide() end),
    }
    if type(s.pending) == "table" and #s.pending > 0 then
        rows[#rows + 1] = row("Approve first request", function() self:action{action="approve", id=s.pending[1].id} end)
    end
    self:showRows("Host tools", rows)
end

function Chat:showRows(title, rows)
    self:closeMenu()
    local ok, menu = safe_call(function() return Menu:new{title=title, item_table=rows, width=Device.screen:getWidth()} end)
    if ok and menu then
        self.menu = menu
        safe_call(function() UIManager:show(menu) end)
    else
        self:message(ok and "Could not draw this screen safely." or menu)
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
    safe_call(function() UIManager:show(dialog); dialog:onShowKeyboard() end)
end

function Chat:endChat()
    self:confirm("End this chat? The host stops hosting. Saved chats and backups remain.", "End chat", function()
        local s = client.state or {}
        local action = s.hosting and s.address == "This Kindle" and "stop-host" or "disconnect"
        if self:action{action=action} then
            shared.ready = nil
            shared.draft = ""
            shared.recipient = nil
            client.state = {stage="idle"}
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
        self.dialog = dialog
        UIManager:show(dialog)
    end)
end

function Chat:stop()
    self:confirm("Stop the background chat service? Saved data remains on the Kindle.", "Stop service", function()
        local ok, err = safe_call(function() return client:stop() end)
        if not ok or not err then
            self:message(ok and "The service did not stop." or err)
            return
        end
        shared.ready = nil
        self:hide()
    end)
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

return Chat
