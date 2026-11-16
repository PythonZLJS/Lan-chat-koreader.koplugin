local InputContainer = require("ui/widget/container/inputcontainer")
local FrameContainer = require("ui/widget/container/framecontainer")
local VerticalSpan = require("ui/widget/verticalspan")
local VerticalGroup = require("ui/widget/verticalgroup")
local TextBoxWidget = require("ui/widget/textboxwidget")
local ScrollTextWidget = require("ui/widget/scrolltextwidget")
local TitleBar = require("ui/widget/titlebar")
local ButtonTable = require("ui/widget/buttontable")
local Device = require("device")
local Font = require("ui/font")
local Geom = require("ui/geometry")
local BB = require("ffi/blitbuffer")
local UIManager = require("ui/uimanager")
local Screen = Device.screen

local Panel = InputContainer:extend{}
function Panel:init()
    local sw,sh=Screen:getWidth(),Screen:getHeight()
    self.screen_w,self.screen_h=sw,sh
    local pad=Screen:scaleBySize(7)
    local width=self.full and sw or math.floor(sw*0.94)
    local target=self.full and sh or math.floor(sh*0.40)
    local border=self.full and 0 or Screen:scaleBySize(2)
    local inner=width-(pad+border)*2
    local model=self.model
    local group=VerticalGroup:new{align="left"}
    local used=(pad+border)*2
    local function add(widget)group[#group+1]=widget;used=used+widget:getSize().h;return widget end
    local function controls(rows,control_width)
        local buttons={}
        for _,row in ipairs(rows)do
            local entries={}
            for _,entry in ipairs(row)do local e=entry
                entries[#entries+1]={text=e.selected and "["..e.text.."]" or e.text,enabled=e.enabled,
                    font_size=e.detail and 22 or (self.full and 19 or 19),font_bold=e.selected or e.detail~=nil,align=e.detail and "left" or "center",avoid_text_truncation=false,
                    callback=function()self.owner:dispatch(e.id,e.value)end}
            end
            buttons[#buttons+1]=entries
        end
        return ButtonTable:new{width=control_width or inner,buttons=buttons,show_parent=self}
    end
    local function label(text,lines)
        if not text or text=="" then return end
        local widget=TextBoxWidget:new{width=inner,text=text,face=Font:getFace("cfont",18),
            alignment="left",height_adjust=true,height=Screen:scaleBySize(28*lines),height_overflow_show_ellipsis=true}
        add(widget)
    end
    self.titlebar=add(TitleBar:new{width=inner,title=model.title,title_face=Font:getFace("cfont",24),title_shrink_font_to_fit=false,show_parent=self,
        close_callback=function()self.owner:hide()end,with_bottom_line=true})
    if self.full then self.navigation=add(controls({model.tabs}));label(model.status,1)end
    local footer=controls(model.tools)
    -- Reserve room for controls before adding descriptions or content.
    local reserve=footer:getSize().h
    if model.alert~="" then label(model.alert,2)
    elseif model.notice~="" then label(model.notice,1)end
    if self.full then label(model.description,2)end
    target=math.min(sh,math.max(target,used+reserve+Screen:scaleBySize(45)))
    local available=math.max(1,target-used-reserve)
    if model.kind=="messages" then
        self.scroll=add(ScrollTextWidget:new{width=inner,height=available,text=model.text or "",
            face=Font:getFace("cfont",self.full and 22 or 19),show_parent=self,top_line_num=self.top_line_num,
            dialog=self,alignment="left",justified=false})
    else
        local cardpad=Screen:scaleBySize(7)
        local function item(entry)
            if not entry.detail then return controls({entry.secondary and {entry,entry.secondary} or {entry}})end
            local cardwidth=inner-2*(cardpad+1)
            local body=VerticalGroup:new{align="left",
                controls({{entry}},cardwidth),
                TextBoxWidget:new{width=cardwidth,text=entry.detail,face=Font:getFace("cfont",17),
                    height=Screen:scaleBySize(56),height_overflow_show_ellipsis=true,alignment="left"}}
            return VerticalGroup:new{FrameContainer:new{padding=cardpad,margin=0,bordersize=1,background=BB.COLOR_WHITE,body},VerticalSpan:new{width=Screen:scaleBySize(7)}}
        end
        local sample=model.rows[1]
        for _,entry in ipairs(model.rows)do if entry.detail then sample=entry;break end end
        local rowheight=Screen:scaleBySize(48)
        if sample then local probe=item(sample);rowheight=probe:getSize().h;if probe.free then probe:free()end end
        local pagerprobe=controls({{{text="Previous"},{text="1 / 2"},{text="Next"}}})
        local pagerheight=pagerprobe:getSize().h
        if pagerprobe.free then pagerprobe:free()end
        local capacity=math.max(1,math.floor((available-pagerheight)/rowheight))
        local pages=math.max(1,math.ceil(#model.rows/capacity))
        self.page=math.min(self.page or 1,pages)
        if #model.rows==0 then
            self.scroll=add(ScrollTextWidget:new{width=inner,height=available,text="Nothing here yet.",face=Font:getFace("cfont",22),show_parent=self})
        else
            local widgets=VerticalGroup:new{align="left"}
            for i=(self.page-1)*capacity+1,math.min(self.page*capacity,#model.rows)do widgets[#widgets+1]=item(model.rows[i])end
            self.actions=widgets
            add(widgets)
            if pages>1 then self.pager=add(controls({{
                {text="Previous",id="page",value=self.page-1,enabled=self.page>1},
                {text=self.page.." / "..pages,id="page",value=self.page,enabled=false},
                {text="Next",id="page",value=self.page+1,enabled=self.page<pages}}}))end
        end
    end
    local gap=target-used-reserve
    if gap>0 then add(TextBoxWidget:new{width=inner,height=gap,text="",face=Font:getFace("cfont",18)})end
    add(footer)
    self[1]=FrameContainer:new{padding=pad,margin=0,bordersize=border,background=BB.COLOR_WHITE,group}
    local actual=self[1]:getSize()
    self.dimen=Geom:new{x=0,y=0,w=actual.w,h=actual.h}
    self.covers_fullscreen=self.full or false
    self.x=self.full and 0 or math.max(0,sw-actual.w)
    self.y=self.full and 0 or math.max(0,sh-actual.h)
    if Device:hasKeys() then self.key_events.ChatHide={{Device.input.group.Back}} end
end
function Panel:onChatHide() self.owner:hide();return true end
function Panel:onSetDimensions()
    if self.screen_w~=Screen:getWidth() or self.screen_h~=Screen:getHeight() then self.owner:render(true) end
end
function Panel:onCloseWidget()
    UIManager:setDirty(nil,"partial",self.dimen)
    if self.free then self:free() end
end
function Panel:handleEvent(event)
    local gesture=event.handler=="onGesture" and event.args and event.args[1]
    local outside=gesture and gesture.pos and self.dimen
        and (gesture.pos.x<self.dimen.x or gesture.pos.y<self.dimen.y
            or gesture.pos.x>=self.dimen.x+self.dimen.w or gesture.pos.y>=self.dimen.y+self.dimen.h)
    if not self.full and outside then
        -- Native popups normally consume outside taps. This one explicitly
        -- gives them to the book, so page turns do not close/disconnect chat.
        if self.owner.ui and self.owner.ui.handleEvent then return self.owner.ui:handleEvent(event) end
        return
    end
    local handled=InputContainer.handleEvent(self,event)
    if handled then return handled end
    if gesture then return true end -- never turn the book through chat itself
end
return Panel
