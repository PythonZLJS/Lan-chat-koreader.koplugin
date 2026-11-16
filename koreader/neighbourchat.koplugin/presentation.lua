-- Pure presentation model: shared by the native widgets and navigation tests.
local M = {}
local function action(text,id,value,enabled)
    return {text=text,id=id,value=value,enabled=enabled~=false}
end
local function row(text,id,value,secondary,enabled)
    local r=action(text,id,value,enabled);r.secondary=secondary;return r
end
local function card(text,detail,id,value,enabled)
    local entry=row(text,id,value,nil,enabled);entry.detail=detail;return entry
end
local function room(s) return s.room and s.room~="" and s.room or "Lobby" end
local function activity(user)
    return user.online and "active" or "away"
end
function M.messages(messages,s,empty)
    local lines={}
    for _,m in ipairs(messages or {}) do
        local sender=(m.user_id and m.user_id~=0 and m.user_id==s.user_id) and "You" or (m.display_name or "Someone")
        local stamp=m.time_label or (m.created_at and m.created_at:match("T(%d%d:%d%d)")) or ""
        local target=m.recipient_id and ("  [private"..(m.recipient_display and m.recipient_display~="" and " to "..m.recipient_display or "").."]") or ""
        lines[#lines+1]=sender..target..(stamp~="" and "   "..stamp or "")
        lines[#lines+1]=m.body or ""
        lines[#lines+1]=""
    end
    return #lines>0 and table.concat(lines,"\n") or empty or "No messages yet. Choose Write to begin."
end
function M.build(s,options)
    options=options or {}
    local screen=options.screen or (s.connected and "chat" or "connect")
    local ready=options.ready and not s.busy
    local model={screen=screen,title="",rows={},tools={},tabs={},busy=s.busy,alert=s.error or "",notice=s.notice or ""}
    model.status=s.connected and ("Connected · "..room(s)..(s.latency_ms and " · "..s.latency_ms.." ms" or ""))
        or (s.stage=="starting" and "Starting chat service…" or "Not connected")
    if s.hosting and s.address=="This Kindle" and s.connected then
        model.status="HOST / ADMIN · "..room(s).." · Discovery "..(s.discoverable and "ON" or "OFF")
    elseif s.hosting and not s.connected then model.status="Hosting · Discovery "..(s.discoverable and "ON" or "OFF") end
    if s.busy then model.status="Working…  "..model.status end
    for _,tab in ipairs({{"connect","Home"},{"chat","Chat"},{"people","People"},{"host","Manage"}}) do
        model.tabs[#model.tabs+1]={text=tab[2],id="navigate",value=tab[1],selected=(screen==tab[1] or (tab[1]=="connect" and (screen=="nearby" or screen=="recent")) or (tab[1]=="host" and (screen=="rooms" or screen=="inbox"))),enabled=true}
    end
    if screen=="chat" then
        model.title="Chat · "..room(s)
        model.kind="messages"
        model.text=M.messages(s.messages,s)
        model.description=""
        if not s.connected then model.text="You are not connected. Open Home to host or find a chat." end
        local recipient=options.recipient or {label="Room",id=""}
        model.tools={
            {action("Write message","write",nil,ready and s.connected),action("To: "..recipient.label,"recipient",nil,ready and s.connected)},
            {action("Older messages","older",s.before,ready and s.connected and s.more),action("Latest messages","latest",nil,ready and s.connected)},
            {action("Search","search",nil,ready and s.connected),action("Files","files",nil,ready and s.connected),action("End chat","end-chat",nil,ready and s.connected)},
        }
    elseif screen=="connect" then
        model.title="Neighbour Chat"
        local stage=options.connect_home and "idle" or s.stage
        if stage=="verify" then
            model.description="Compare this check code with Invite people on the host:\n"..(s.code or "").."\n"..(s.address or "")
            model.rows={row("Codes match — continue","verify",nil,nil,ready),row("Choose another host","connect-home")}
        elseif stage=="setup" then
            model.description="Start your own chat. Choose a host password of at least 10 characters."
            model.rows={row("Choose host password","setup",nil,nil,ready),row("Choose another host","connect-home")}
        elseif stage=="login" then
            model.description="Verified host: "..(s.address or "").."\nSign in with an approved account."
            model.rows={row("Sign in","login",nil,nil,ready),row("Request a new account","register",nil,nil,ready),row("Choose another host","connect-home")}
        elseif stage=="locked" or s.vault_locked then
            model.description="Saved content is locked. Unlock it to host on this Kindle."
            model.rows={row("Unlock saved content","unlock",nil,nil,ready),row("Join a different host","join",nil,nil,ready)}
        else
            model.description="One app: host on this Kindle or find a nearby chat."
            model.rows={
                card(s.hosting and "Your hosted chat" or "Host a chat","This Kindle is the server. You are the admin.","host",nil,ready),
                card("Find a chat","Scan your Wi-Fi and tap a host. No IP typing.","scan",nil,ready),
                card("Recent chats ("..#(s.recent_hosts or {})..")","Reconnect to a trusted host you used before.","navigate","recent",ready),
            }
            if s.connected then table.insert(model.rows,1,card("Continue chatting",s.address=="This Kindle" and "You are hosting this chat." or (s.address or "Connected"),"navigate","chat",ready))end
        end
        model.tools={{action("Enter IP manually","join",nil,ready),action("Pop-up","popup")},
            {action("Disconnect","disconnect",nil,ready and s.connected),action("Stop","stop"),action("Retry","retry")}}
    elseif screen=="nearby" then
        model.title="Find a chat"
        model.description=s.busy and "Scanning your Wi-Fi… This takes about 3 seconds." or
            "Ask the host to turn discovery on. Choose their chat below."
        for _,host in ipairs(s.nearby or {})do
            model.rows[#model.rows+1]=card(host.name,host.address:gsub("^https://","").." · Tap to connect","join-nearby",host.address,ready)
        end
        if #model.rows==0 then
            model.kind="messages"
            model.text=s.busy and "Looking for nearby chats…" or (s.scanned and "No chats found.\n\nCheck both Kindles use the same Wi-Fi and the host has turned discovery on. Some guest networks block discovery. You can still use a recent or manual address." or "Tap Scan to look for chats on this Wi-Fi.")
        end
        model.tools={{action("Scan again","scan",nil,ready),action("Home","navigate","connect")}}
    elseif screen=="recent" then
        model.title="Recent chats"
        model.description="Addresses are remembered. Passwords are never saved."
        for _,address in ipairs(s.recent_hosts or {})do
            model.rows[#model.rows+1]=row(address:gsub("^https://",""),"reconnect",address,action("Forget","forget",address,ready),ready)
        end
        if #model.rows==0 then model.kind="messages";model.text="No saved chats yet. Find a chat and verify its check code once; it will appear here next time." end
        model.tools={{action("Find a chat","scan",nil,ready),action("Home","navigate","connect")}}
    elseif screen=="people" then
        model.title="People"
        model.description="Choose a person to write privately. Active means seen within the last 90 seconds."
        if not s.connected then model.description="Connect to a host first." end
        for _,user in ipairs(s.users or {}) do
            model.rows[#model.rows+1]=row(user.display_name.."  ·  "..activity(user),"person",user,nil,ready)
        end
        model.tools={{action("Refresh","people",nil,ready and s.connected),action("Rooms","navigate","rooms",ready and s.connected),action("Message host","message-host",nil,ready and s.connected)}}
    elseif screen=="rooms" then
        model.title="Rooms"
        model.description="Your room: "..room(s)..". "..(s.admin and "Choose a room to manage it or move people." or "The host assigns rooms. Choose one to ask to move.")
        if not s.rooms or #s.rooms==0 then model.description="Room features require a host running Neighbour Chat 6.0 or newer." end
        for _,name in ipairs(s.rooms or {}) do model.rows[#model.rows+1]=row((name==room(s) and "Current: " or "")..name,"room",name,nil,ready and s.connected) end
        model.tools={{action("Refresh","people",nil,ready and s.connected),action("Create room","create-room",nil,ready and s.connected and s.admin)}}
    elseif screen=="host" then
        model.title="Manage chat"
        model.description=s.hosting and ("This Kindle is the server · Discovery "..(s.discoverable and "ON" or "OFF")) or "Host a chat on this Kindle to manage its discovery and members."
        if s.hosting then
            model.rows[#model.rows+1]=card(s.discoverable and "Turn discovery off" or "Turn discovery on",
                s.discoverable and "Hide from app scans. Connected people stay connected." or "Let people on your Wi-Fi find this chat.",
                s.discoverable and "discovery-off" or "discovery-on",nil,ready)
            model.rows[#model.rows+1]=row("Invite / check code","invite",nil,nil,ready)
        end
        if s.connected and s.admin then
            model.rows[#model.rows+1]=row("Join requests ("..#(s.pending or {})..")","requests",nil,nil,ready)
            model.rows[#model.rows+1]=row("Accounts / kick / ban","accounts",nil,nil,ready)
            model.rows[#model.rows+1]=row("Rooms","navigate","rooms",nil,ready)
            model.rows[#model.rows+1]=row("Private host inbox","inbox",nil,nil,ready)
        elseif s.hosting then
            model.rows[#model.rows+1]=card("Open as host / admin","Return to this Kindle's chat and manage its members.","host",nil,ready)
        else
            model.rows[#model.rows+1]=card("Host a chat","Start a server here and become its admin.","host",nil,ready)
        end
        model.tools={{action("Refresh","admin",nil,ready and s.connected and s.admin),action("End chat","end-chat",nil,ready and s.connected),action("Stop hosting","stop-host",nil,ready and s.hosting)}}
    elseif screen=="inbox" then
        model.title="Host inbox";model.kind="messages";model.description="Private messages addressed to the host"
        model.text=M.messages(s.inbox,s,"No messages in the host inbox.")
        model.tools={{action("Refresh inbox","inbox",nil,ready),action("Host tools","navigate","host")}}
    elseif screen=="search" then
        model.title="Search results";model.kind="messages";model.description="Search: "..(s.query or "")
        model.text=M.messages(s.results,s,"No matching messages in your room or private conversations.")
        model.tools={{action("Search again","search",nil,ready),action("Back to chat","navigate","chat")}}
    end
    if not model.kind then model.kind="actions" end
    if options.popup then
        model.title=s.connected and ("Chat · "..room(s)) or "Neighbour Chat"
        model.tabs={};model.kind="messages"
        model.text=s.connected and M.messages(s.messages,s) or "Open Home to host a chat or find one nearby."
        model.description=""
        model.tools={{action(s.connected and "Write" or "Connect",s.connected and "write" or "connect-full",nil,ready),action("Expand","full"),action("Hide","hide")}}
    end
    return model
end
return M
