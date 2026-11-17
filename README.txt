NEIGHBOUR CHAT 7.1 - KOREADER ONLY

Ready-to-copy folder: dist/neighbourchat.koplugin
Download ZIP: dist/NeighbourChat-KOReader.zip
Installation/use/removal: INSTALL.txt

Copy the plugin folder into KOReader/plugins, restart KOReader, and open
Tools -> More tools -> Neighbour Chat. No installer or separate Kindle app.
Remade Home/Chat/People/Manage navigation with large Host / Find cards.
One plugin is both client and server: hosting makes this Kindle's owner admin.
Admin sign-in is restricted to the machine running the server; remote member
devices cannot gain host controls with the admin password.
Chat includes an explicit End chat action: participants disconnect, while the
host also stops hosting; saved messages, accounts, files and backups remain.
Opt-in Wi-Fi discovery finds named chats without typing IP addresses. Hosts
can turn discovery off without disconnecting people. It defaults off after
service restarts. This hides app discovery, not the server from port scanners.
Recent trusted IP addresses, private chat/files, rooms and moderation remain.
Original Communication-Script TCP/Fernet clients use a different protocol.
Full screen and a reading popup share a persistent background connection.
Hide keeps it running; Disconnect, Stop hosting and Stop completely are
separate controls. Existing chats and the central backups folder are retained.

Source: native/ (service), koreader/neighbourchat.koplugin/ (native Lua UI),
koreader/tests/ and tools/build-koreader.py. See VERIFICATION.txt for test scope.
The service retains the API and old-data compatibility code needed for existing
LAN clients and data. The KOReader runtime stores all new data below
neighbourchat.koplugin/data and does not register or launch a Mesquite app.

Compilation caches, old release archives, old launchers/installers and downloaded
PC toolchains are removed from this finished workspace. Build instructions list
the PC prerequisites for rebuilding. The copied plugin needs none of them.
Physical Kindle operation remains unverified.

7.1 fixes private cache/draft isolation, moderation/session races, room-change
privacy, discovery recovery and oversized reader replies. See SECURITY-REVIEW.txt.
