"""Build a fake release tarball: mktar.py OUT MODE VERSION. MODE: ok | dotdot | dotdot_cf | notcf | symlink."""
import io
import sys
import tarfile

out, mode, version = sys.argv[1:4]
script = ("#!/bin/sh\necho %s\n" % version).encode()


def add(archive, name, body=script):
    info = tarfile.TarInfo(name)
    info.size = len(body)
    info.mode = 0o755
    archive.addfile(info, io.BytesIO(body))


with tarfile.open(out, "w:gz") as archive:
    if mode == "ok":
        add(archive, "cf")
        add(archive, "README.md", b"readme\n")
    elif mode == "dotdot":
        add(archive, "cf")
        add(archive, "../evil")
    elif mode == "dotdot_cf":
        add(archive, "../cf")
    elif mode == "notcf":
        add(archive, "bin/cf")
    elif mode == "symlink":
        info = tarfile.TarInfo("cf")
        info.type = tarfile.SYMTYPE
        info.linkname = "/bin/sh"
        archive.addfile(info)
    else:
        sys.exit("unknown mode " + mode)
