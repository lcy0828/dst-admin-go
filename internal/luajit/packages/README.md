This directory embeds the Debian 12 compatible Linux x64 LuaJIT build in both
the local Runtime and Agent. Build it with tools/linux/package-admin.py and
update manifest.json with its SHA-256, size, revision and compatibility channel.

upstream.json contains only official stable release metadata. The selected node
can explicitly refresh it from GitHub and download the official Linux ZIP on
installation. Catalog reads never extract, download, or install packages.

Official packages do not need a private runtime-mode contract. The installer
validates archive paths, ELF architecture, package structure and checksum, then
checks system library dependencies on the target node before replacing files.
The compatibility build retains necessary Linux fixes and uses upstream VM/JIT
configuration. Controller transfer is a separate, optional path.
