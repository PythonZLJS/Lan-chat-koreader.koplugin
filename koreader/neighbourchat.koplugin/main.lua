local Device = require("device")
if not Device:isKindle() then return {disabled=true} end
local WidgetContainer = require("ui/widget/container/widgetcontainer")
local UIManager = require("ui/uimanager")
local NetworkMgr = require("ui/network/manager")
local ButtonDialog = require("ui/widget/buttondialog")
local InputDialog = require("ui/widget/inputdialog")
local MultiInputDialog = require("ui/widget/multiinputdialog")
local ConfirmBox = require("ui/widget/confirmbox")
local InfoMessage = require("ui/widget/infomessage")
local TextViewer = require("ui/widget/textviewer")
local Menu = require("ui/widget/menu")
local Dispatcher = require("dispatcher")
local source=debug.getinfo(1,"S").source
local path=source:match("^@(.+)/[^/]+$") or "plugins/neighbourchat.koplugin"
local Client=dofile(path.."/client.lua")
local Panel=dofile(path.."/panel.lua")
local Presentation=dofile(path.."/presentation.lua")
local shared=package.loaded["neighbourchat.session"]
if not shared then
    shared={client=Client.new(path),full=false,draft=""}
    package.loaded["neighbourchat.session"]=shared
end
local client=shared.client
local Chat=WidgetContainer:extend{name="neighbourchat",is_doc_only=false}

local function button(text,callback,enabled) return {text=text,callback=callback,enabled=enabled~=false} end
local function contextKey()
    local s=client.state or {}
    return table.concat({s.context or "",s.address or "",tostring(s.user_id or 0),s.room or "",tostring(s.connected==true)},"\0")
end
local function clearDraft()
    shared.draft="";shared.send_token=nil;shared.sent_body=nil;shared.sent_recipient=nil;shared.recipient=nil
end
function Chat:guard(callback,expected)
    expected=expected or contextKey()
    return function(...)
        if expected~=contextKey()then self:message("The chat, account or room changed. Reopen this screen.");return end
        return callback(...)
    end
end
function Chat:syncContext()
    local key=contextKey()
    if shared.context_key~=key then clearDraft();shared.context_key=key end
end
function Chat:init()
    self.ui.menu:registerToMainMenu(self)
    self:onDispatcherRegisterActions()
end
function Chat:onDispatcherRegisterActions()
    Dispatcher:registerAction("neighbourchat_popup",{category="none",event="NeighbourChatPopup",title="Neighbour Chat: reading popup",general=true})
    Dispatcher:registerAction("neighbourchat_full",{category="none",event="NeighbourChatFull",title="Neighbour Chat: full screen",general=true})
end
function Chat:addToMainMenu(items)
    local function open(full)
        if self.ui.menu.onTapCloseMenu then self.ui.menu:onTapCloseMenu()
        elseif self.ui.menu.onCloseFileManagerMenu then self.ui.menu:onCloseFileManagerMenu() end
        UIManager:nextTick(function() self:open(full) end)
    end
    items.neighbourchat={text="Neighbour Chat",sorting_hint="more_tools",sub_item_table={
        {text="Reading popup",callback=function() open(false) end},
        {text="Full screen",callback=function() open(true) end},
        {text="End chat",callback=function() self:endChat() end},
        {text="Disconnect from chat",callback=function() self:action{action="disconnect"} end},
        {text="Stop chat completely",callback=function() self:stop() end},
    }}
end
function Chat:onNeighbourChatPopup() self:open(false);return true end
function Chat:onNeighbourChatFull() self:open(true);return true end
function Chat:message(text) UIManager:show(InfoMessage:new{text=text,timeout=5}) end
function Chat:hide()
    if self.poll then UIManager:unschedule(self.poll);self.poll=nil end
    if self.view then local view=self.view;self.view=nil;UIManager:close(view) end
    self.signature=nil
    if shared.owner==self then shared.owner=nil end
    -- Deliberately no disconnect, logout, daemon stop, or LAN request here.
end
function Chat:open(full)
    if shared.owner and shared.owner~=self then shared.owner:hide() end
    self:hide();shared.owner=self;shared.full=full==true;self.start_error=nil
    self:render(true)
    self.poll=function() self:tick() end
    UIManager:scheduleIn(0.1,self.poll)
end
function Chat:tick()
    if not self.view then return end
    if not shared.ready then
        local ready,err=client:start()
        if ready then shared.ready=true
        elseif ready==nil then self.start_error=err;client.state={stage="idle",error=err}
        else client.state={stage="starting"} end
    end
    if shared.ready then
        local state,err=client:readState()
        if not state then
            client.state.error=err;self.local_failures=(self.local_failures or 0)+1
            if self.local_failures>=3 then shared.ready=nil;client.token=nil;client.starting=nil;client.state={stage="idle",error=err} end
        else self.local_failures=0;self:syncContext() end
        if state and shared.send_token and state.sent_token==shared.send_token then
            if not shared.sent_body or shared.draft==shared.sent_body then shared.draft="" end
            shared.send_token=nil
        end
    end
    if self.view and not self.dialog then self:render() end
    if self.view and not self.start_error then UIManager:scheduleIn(shared.ready and 2 or 0.5,self.poll) end
end
function Chat:navigate(screen)
    shared.screen=screen;self.page=1;self.connect_home=screen=="connect" and client.state.connected or false
    self:render(true)
    if shared.ready and client.state.connected and not client.state.busy then
        if screen=="people" or screen=="rooms" then self:action{action="people"}
        elseif screen=="host" and client.state.admin then self:action{action="admin"} end
    end
end
function Chat:render(force)
    local s=client.state or {}
    if self.previous_stage~=s.stage then
        if s.stage=="verify" or s.stage=="login" or s.stage=="setup" or s.stage=="locked" then
            shared.screen="connect";self.connect_home=false
        elseif s.connected and self.previous_stage~="connected" then shared.screen=self.after_connect or "chat";self.after_connect=nil end
        self.previous_stage=s.stage;self.page=1
    end
    local model=Presentation.build(s,{screen=shared.screen,ready=shared.ready,popup=not shared.full,
        connect_home=self.connect_home,recipient=shared.recipient})
    local json=require("json")
    local signature=json.encode(model)..tostring(shared.full)..tostring(self.page or 1)
    if not force and self.signature==signature then return end
    local top
    if self.view then
        if self.view.model.screen==model.screen and self.view.scroll then
            top=self.view.scroll.text_widget and self.view.scroll.text_widget.virtual_line_num
        end
        local old=self.view;self.view=nil;UIManager:close(old)
    end
    self.signature=signature
    local view=Panel:new{owner=self,full=shared.full,model=model,text=model.text,top_line_num=top,page=self.page or 1}
    self.view=view
    UIManager:show(view,"partial",nil,view.x,view.y)
end
function Chat:dispatch(id,value)
    if id=="navigate" then self:navigate(value)
    elseif id=="page" then self.page=value;self:render(true)
    elseif id=="popup" then self:open(false)
    elseif id=="full" then self:open(true)
    elseif id=="hide" then self:hide()
    elseif id=="connect-full" then shared.screen="connect";self:open(true)
    elseif id=="connect-home" then self.connect_home=true;self.page=1;self:render(true)
    elseif id=="retry" then self.start_error=nil;shared.ready=nil;client.starting=nil;self:open(shared.full)
    elseif id=="stop" then self:stop()
    elseif id=="join" then self.after_connect=nil;self.connect_home=false;self:join()
    elseif id=="host" then
        self.connect_home=false;self.after_connect="host";shared.screen="host"
        self:online(function()self:action{action="host"}end)
    elseif id=="scan" then
        self.after_connect=nil;shared.screen="nearby";self.page=1;self:online(function()self:action{action="scan"}end)
    elseif id=="join-nearby" then self.after_connect=nil;self.connect_home=false;self:online(function()self:action{action="join-nearby",address=value}end)
    elseif id=="discovery-on" then
        self:fields("Turn discovery on",{{description="Chat name shown nearby",text=client.state.discovery_name and client.state.discovery_name~="" and client.state.discovery_name or "Kindle chat"}},function(v)self:action{action="discovery-on",name=v[1]}end,"Make discoverable")
    elseif id=="discovery-off" then self:action{action="discovery-off"}
    elseif id=="reconnect" then self.after_connect=nil;self.connect_home=false;self:online(function()self:action{action="reconnect",address=value}end)
    elseif id=="forget" then self:confirm("Forget this saved address?\n"..value,function()self:action{action="forget",address=value}end,"Forget")
    elseif id=="older" then self:action{action="older",before=value}
    elseif id=="person" then shared.recipient={id=tostring(value.id),label=value.display_name};self:write()
    elseif id=="recipient" then self:recipients()
    elseif id=="message-host" then self:writeHost()
    elseif id=="room" then self:room(value)
    elseif id=="create-room" then self:fields("Create room",{{description="Room name",text=""}},function(v)self:action{action="create-room",room=v[1]}end)
    elseif id=="accounts" then self:accounts()
    elseif id=="search" then self:fields("Search messages",{{description="Words to find",text=client.state.query or ""}},function(v)
        shared.screen="search";self:action{action="search",query=v[1]}
    end)
    elseif id=="inbox" then shared.screen="inbox";self:action{action="inbox"}
    elseif id=="disconnect" then shared.screen="connect";self.connect_home=true;shared.recipient=nil;self:action{action=id}
    elseif id=="end-chat" then self:endChat()
    elseif id=="stop-host" then self:confirm("Stop hosting on this Kindle? Everyone connected here will be disconnected.",function()self:action{action=id}end,"Stop hosting")
    elseif self[id] then self[id](self)
    else self:action{action=id} end
end
function Chat:confirm(text,callback,label)
    UIManager:show(ConfirmBox:new{text=text,ok_text=label or "Confirm",ok_callback=self:guard(callback)})
end
function Chat:menu(title,rows)
    local dialog
    local items={}
    for _,entry in ipairs(rows)do
        local item=entry
        items[#items+1]={text=item[1],callback=self:guard(function()UIManager:close(dialog);item[2]()end)}
    end
    dialog=Menu:new{title=title,item_table=items};UIManager:show(dialog)
end
function Chat:recipients()
    local rows={{"Room: "..(client.state.room or "Lobby"),function()shared.recipient=nil;self:render(true)end}}
    for _,user in ipairs(client.state.users or {})do local u=user
        rows[#rows+1]={u.display_name,function()shared.recipient={id=tostring(u.id),label=u.display_name};self:render(true)end}
    end
    self:menu("Message recipient",rows)
end
function Chat:writeHost(prefix)
    local host
    for _,u in ipairs(client.state.users or {})do if u.username=="server_admin_system" or u.username=="admin" then host=u;break end end
    if host then shared.recipient={id=tostring(host.id),label="Host"}
    else shared.recipient=nil;prefix="@admin "..(prefix or "") end
    if prefix then shared.draft=prefix end
    self:write()
end
function Chat:room(name)
    if not client.state.admin then self:writeHost("Please move me to room "..name..".");return end
    local rows={{"Move me here",function()self:action{action="move-user",id=client.state.user_id,room=name}end},
        {"Move a person here",function()self:showAfter({action="admin"},function(s)
            local people={}
            for _,u in ipairs(s.accounts or {})do local user=u
                if user.status=="approved" then people[#people+1]={user.display_name.." ("..(user.room or "Lobby")..")",function()self:action{action="move-user",id=user.id,room=name}end}end
            end
            self:menu("Move to "..name,people)
        end)end}}
    if name~="Lobby" then rows[#rows+1]={"Delete this room",function()self:confirm("Delete "..name.."? Members return to Lobby. Its public history is archived.",function()self:action{action="delete-room",room=name}end,"Delete room")end}end
    self:menu(name,rows)
end
function Chat:accounts()
    self:showAfter({action="admin"},function(s)
        local rows={}
        for _,u in ipairs(s.accounts or {})do local user=u
            if user.id~=s.user_id then rows[#rows+1]={user.display_name.." · "..user.status.." · "..(user.room or "Lobby"),function()
                local actions={}
                if user.status=="banned" then actions={{"Unban account and its saved IP ban",function()self:action{action="unban",id=user.id}end}}
                else
                    actions={{"Write privately",function()shared.recipient={id=tostring(user.id),label=user.display_name};self:write()end},
                        {"Kick (can sign in again)",function()self:confirm("Disconnect "..user.display_name.." now?",function()self:action{action="kick",id=user.id}end,"Kick")end},
                        {"Ban account and last known Wi-Fi IP",function()self:confirm("Ban "..user.display_name.."? A known Wi-Fi IP is also blocked, including other accounts sharing it.",function()self:action{action="ban",id=user.id}end,"Ban")end}}
                end
                self:menu(user.display_name,actions)
            end}end
        end
        if #rows==0 then self:message("No other accounts yet.")else self:menu("Manage accounts",rows)end
    end)
end
function Chat:action(value)
    if not shared.ready then self:message("Open the chat window first.");return false end
    local ok,err=client:action(value)
    if not ok then self:message(err);return false end
    if value.action=="disconnect" or value.action=="end-chat" then shared.draft="";shared.send_token=nil;shared.recipient=nil end
    client.state.busy=true;client.state.error="";client.state.notice=""
    if self.view then self:render(true) end
    return true
end
function Chat:online(callback) NetworkMgr:runWhenConnected(callback) end
function Chat:fields(title,fields,done,submit)
    done=self:guard(done)
    self.dialog=true
    local dialog
    local function close() UIManager:close(dialog);self.dialog=nil end
    dialog=MultiInputDialog:new{title=title,fields=fields,buttons={{
        button("Cancel",close),button(submit or title,function() local values=dialog:getFields();close();done(values) end),
    }}}
    local original=dialog.onCloseWidget
    dialog.onCloseWidget=function(widget)
        self.dialog=nil
        if original then return original(widget) end
    end
    UIManager:show(dialog);dialog:onShowKeyboard()
end
function Chat:join()
    self:online(function()
        self:fields("Host Wi-Fi address",{{text=shared.last_address or "",hint="192.168.1.42:8000"}},function(v)
            shared.last_address=v[1];self:action{action="probe",address=v[1]}
        end)
    end)
end
function Chat:verify()
    local s=client.state
    UIManager:show(ConfirmBox:new{text="Does this code match the host's Invite people screen?\n\n"..(s.code or ""),ok_text="Codes match",ok_callback=self:guard(function() self:action{action="confirm",token=s.verify_token} end)})
end
function Chat:login()
    self:fields("Sign in",{{description="Username",text=client.state.address=="This Kindle" and "admin" or (shared.username or "")},
        {description="Password",text="",text_type="password"}},function(v)
        shared.username=v[1];self:action{action="login",username=v[1],password=v[2]}
    end)
end
function Chat:setup()
    self:fields("Choose host password",{{description="Password (10+ characters)",text="",text_type="password"},
        {description="Repeat password",text="",text_type="password"}},function(v) self:action{action="setup",password=v[1],confirm=v[2]} end)
end
function Chat:unlock()
    self:fields("Unlock saved content",{{description="Encryption password",text="",text_type="password"}},function(v) self:action{action="unlock",password=v[1]} end)
end
function Chat:register()
    self:fields("Request an account",{{description="Host join code",text=""},{description="Display name",text=""},
        {description="Username",text=""},{description="Password (10+ characters)",text="",text_type="password"}},function(v)
        self:action{action="register",join_code=v[1],display=v[2],username=v[3],password=v[4]}
    end)
end
function Chat:write()
    local expected=contextKey()
    local request_context=client.state.context
    local function compose(recipient,label)
        self.dialog=true
        local dialog
        local function keep()
            if expected==contextKey()then shared.draft=dialog:getInputText()end;UIManager:close(dialog);self.dialog=nil
        end
        dialog=InputDialog:new{title="Message to "..label,input=shared.draft,input_hint="Write your message...",
            allow_newline=true,text_height=Device.screen:scaleBySize(100),buttons={{button("Keep draft",keep),button("Send",function()
                if expected~=contextKey()or not client.state.connected then UIManager:close(dialog);self:message("The chat, account or room changed. Your message was not sent.");return end
                shared.draft=dialog:getInputText()
                if shared.draft=="" then return end
                if not shared.send_token or shared.sent_body~=shared.draft or shared.sent_recipient~=recipient then
                    -- This is an opaque duplicate-prevention key, not a credential.
                    shared.send_token=string.format("reader-%s-%s-%s",os.time(),math.random(100000,999999),math.random(100000,999999))
                    shared.sent_body=shared.draft;shared.sent_recipient=recipient
                end
                keep();self:action{action="send",body=shared.draft,recipient=recipient,token=shared.send_token,context=request_context}
            end)}}}
        local original=dialog.onCloseWidget
        dialog.onCloseWidget=function(widget)
            if expected==contextKey()then shared.draft=widget:getInputText()end;self.dialog=nil
            if original then return original(widget) end
        end
        UIManager:show(dialog);dialog:onShowKeyboard()
    end
    local recipient=shared.recipient or {id="",label="room "..(client.state.room or "Lobby")}
    compose(recipient.id,recipient.label)
end
function Chat:files()
    self:showAfter({action="files"},function(s)
        local dialog
        local rows={{text="Share a file",callback=self:guard(function()
            UIManager:close(dialog)
            local chooser=require("ui/widget/pathchooser"):new{path="/mnt/us/documents",select_file=true,select_directory=false,
                onConfirm=self:guard(function(file)
                    local recipients
                    local options={{text="Room: "..(client.state.room or "Lobby"),callback=self:guard(function() UIManager:close(recipients);self:action{action="upload",path=file,recipient=""} end)}}
                    for _,user in ipairs(client.state.users or {}) do
                        local u=user
                        options[#options+1]={text=u.display_name,callback=self:guard(function() UIManager:close(recipients);self:action{action="upload",path=file,recipient=tostring(u.id)} end)}
                    end
                    recipients=Menu:new{title="Share with",item_table=options};UIManager:show(recipients)
                end)}
            UIManager:show(chooser)
        end)}}
        for _,file in ipairs(s.files or {}) do
            local f=file
            rows[#rows+1]={text=f.original_name..(f.recipient_id and " (private)" or ""),callback=self:guard(function()
                UIManager:close(dialog)
                UIManager:show(ConfirmBox:new{text="Save "..f.original_name.." to NeighbourChat-Downloads? Downloaded copies are ordinary files outside saved-content encryption.",ok_text="Download",
                    ok_callback=self:guard(function() self:action{action="download",id=f.id} end)})
            end)}
        end
        dialog=Menu:new{title="Shared files",item_table=rows};UIManager:show(dialog)
    end)
end
function Chat:showAfter(action,callback)
    callback=self:guard(callback)
    if not self:action(action)then return end
    local attempts=0
    local wait
    wait=function()
        if not self.view then return end
        local s=client:readState();attempts=attempts+1
        if s and not s.busy then
            if s.error and s.error~="" then self:message(s.error) else callback(s) end
        elseif attempts<80 then UIManager:scheduleIn(0.25,wait) else self:message("Still working. Reopen chat shortly.") end
    end
    UIManager:scheduleIn(0.25,wait)
end
function Chat:invite()
    self:showAfter({action="invite"},function(s)
        UIManager:show(TextViewer:new{title="Invite people",text="Wi-Fi address:\n"..table.concat(s.addresses or {},"\n").."\n\nJoin code:\n"..(s.join_code or "").."\n\nServer check code:\n"..(s.code or "").."\n\nApprove new accounts in Manage → Join requests."})
    end)
end
function Chat:requests()
    self:showAfter({action="admin"},function(s)
        local dialog;local rows={}
        for _,user in ipairs(s.pending or {}) do
            local u=user
            rows[#rows+1]={button("Approve "..u.display_name,self:guard(function() UIManager:close(dialog);self:action{action="approve",id=u.id} end)),
                button("Deny",self:guard(function() UIManager:close(dialog);self:action{action="deny",id=u.id} end))}
        end
        if #rows==0 then self:message("No account requests waiting.");return end
        dialog=ButtonDialog:new{title="Account requests",buttons=rows};UIManager:show(dialog)
    end)
end
function Chat:endChat()
    self:confirm("End this chat? You will be disconnected, and hosting will stop if this Kindle is the host. Saved messages and backups are kept.",
        function()
            -- Keep the release plugin compatible with older bundled services:
            -- ending a hosted chat is the existing stop-host operation; a
            -- participant only needs the existing disconnect operation.
            local action = client.state.hosting and client.state.address == "This Kindle" and "stop-host" or "disconnect"
            if self:action{action=action} then
                shared.ready=nil
                shared.draft=""
                shared.send_token=nil
                shared.recipient=nil
                shared.screen="connect"
                shared.context_key=nil
                self.connect_home=true
            end
        end,"End chat")
end
function Chat:stop()
    UIManager:show(ConfirmBox:new{text="Disconnect, stop hosting and stop the background chat service? Saved chats and backups are kept.",
        ok_text="Stop completely",ok_callback=function()
            local ok,err=client:stop()
            if not ok then self:message(err);return end
            shared.ready=nil;shared.draft="";shared.send_token=nil;self:hide()
        end})
end
function Chat:onSuspend() if shared.ready then client:action{action="pause"} end end
function Chat:onResume() if shared.ready then client:action{action="resume"} end end
function Chat:onCloseDocument() self:hide() end
function Chat:onSetDimensions() if self.view then self:render(true) end end
return Chat
