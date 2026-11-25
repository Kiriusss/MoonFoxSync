#!/usr/bin/env python3
"""Package the Windows x64 release on Windows with WiX 3 candle/light."""
import argparse
from pathlib import Path
import shutil
import struct
import subprocess
import tempfile
import uuid
import xml.etree.ElementTree as ET


def main():
    root = Path(__file__).resolve().parents[1]
    windows = root / "client" / "windows"
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", type=Path, default=root / "dist" / "windows-amd64")
    parser.add_argument("--output", type=Path, default=root / "dist" / "MoonFoxSync-Windows-x64.msi")
    args = parser.parse_args()
    raw_version = ET.parse(windows / "XinhuSync.csproj").findtext(".//Version")
    components = [int(x) for x in raw_version.split(".")]
    if len(components) not in (3, 4) or any(x < 0 for x in components) or any(x > 255 for x in components[:2]) or components[2] > 65535 or any(components[3:]):
        parser.error("the project requires a three-field MSI release version")
    version = ".".join(map(str, components[:3]))
    product_id = str(uuid.uuid5(uuid.UUID("32e34d01-9ea5-4d9a-ad7c-40d0ca41eb9d"), "x64-" + version)).upper()

    if not (shutil.which("candle") and shutil.which("light")):
        parser.error("add the WiX Toolset 3 bin directory to PATH (requires WixUIExtension and WixUtilExtension)")
    paths = {"gui.exe": "心狐同步.exe", "gui.exe.config": "心狐同步.exe.config",
             "kdrive.exe": "kdrive.exe", "kdrive-agent.exe": "kdrive-agent.exe"}
    for source in paths.values():
        if not (args.input / source).is_file():
            parser.error("missing Windows release file: " + source)
    for name in ("kdrive.exe", "kdrive-agent.exe"):
        with (args.input / name).open("rb") as exe:
            exe.seek(0x3c)
            exe.seek(struct.unpack("<I", exe.read(4))[0])
            if exe.read(6) != b"PE\0\0\x64\x86":
                parser.error(name + " must be a Windows x64 executable")

    output = args.output.resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="moonfox-msi-", dir=output.parent) as stage:
        stage = Path(stage)
        for target, source in paths.items():
            shutil.copyfile(args.input / source, stage / target)
        for source in ("Installer.wxs", "xinhu.ico"):
            shutil.copyfile(windows / source, stage / source)
        defines = ["ProductId=" + product_id, "ProductVersion=" + version]
        subprocess.run(["candle", "-nologo", "-arch", "x64", *["-d" + d for d in defines],
                        "Installer.wxs"], cwd=stage, check=True)
        subprocess.run(["light", "-nologo", "-ext", "WixUIExtension", "-ext", "WixUtilExtension",
                        "-cultures:zh-CN",
                        "-o", "product.msi", "Installer.wixobj"], cwd=stage, check=True)
        (stage / "product.msi").replace(output)
    print(output)


if __name__ == "__main__":
    main()
