#!/usr/bin/env python3
"""Package the Windows x64 release with wixl (msitools) or WiX 3 candle/light."""
import argparse
from pathlib import Path
import shutil
import struct
import subprocess
import tempfile
import uuid
import xml.etree.ElementTree as ET


def write_localized_names(stage, source):
    # wixl 0.106 initially emits a neutral string pool regardless of Product/@Codepage.
    # Recode the MSI and apply the original localized shell and payload names.
    root = ET.parse(source).getroot()
    ns = {"w": "http://schemas.microsoft.com/wix/2006/wi"}
    product = root.find("w:Product", ns)
    codepage = product.attrib["Codepage"]
    (stage / "codepage.idt").write_bytes(("\r\n\r\n" + codepage + "\t_ForceCodepage\r\n").encode("ascii"))
    subprocess.run(["msibuild", "product.msi", "-s", "Moon Fox Sync", "Moon Fox Sync",
                    "x64;" + product.attrib["Language"], "{" + str(uuid.uuid4()).upper() + "}"],
                   check=True, cwd=stage)
    commands = ["msibuild", "product.msi", "-i", "codepage.idt"]
    for element, column in (("File", "FileName"), ("Shortcut", "Name")):
        for row in product.findall(".//w:" + element, ns):
            if "Name" in row.attrib:
                value = row.attrib["Name"].replace("'", "''")
                commands += ["-q", f"UPDATE {element} SET {column} = '{value}' WHERE {element} = '{row.attrib['Id']}'"]
    # Directory-based EXE custom actions are not implemented by wixl 0.106.
    action = product.find("w:CustomAction[@Id='RemoveLogin']", ns)
    target = action.attrib["ExeCommand"].replace("'", "''")
    commands += ["-q", "INSERT INTO CustomAction (Action, Type, Source, Target) VALUES "
                 + f"('RemoveLogin', 98, '{action.attrib['Directory']}', '{target}')"]
    subprocess.run(commands, cwd=stage, check=True)


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

    if not shutil.which("wixl") and not (shutil.which("candle") and shutil.which("light")):
        parser.error("install msitools (wixl) or add WiX Toolset 3 to PATH")
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
        if shutil.which("wixl"):
            ns = {"w":"http://schemas.microsoft.com/wix/2006/wi"}
            ET.register_namespace("", ns["w"])
            wx = ET.parse(stage / "Installer.wxs")
            prod = wx.getroot().find("w:Product", ns)
            prod.remove(prod.find("w:CustomAction[@Id='RemoveLogin']", ns))
            wx.write(stage / "portable.wxs", encoding="utf-8", xml_declaration=True)
            subprocess.run(["wixl", "-a", "x64", "-D", defines[0], "-D", defines[1],
                            "-o", "product.msi", "portable.wxs"], cwd=stage, check=True)
            write_localized_names(stage, stage / "Installer.wxs")
        else:
            subprocess.run(["candle", "-nologo", "-arch", "x64", *["-d"+d for d in defines],
                            "Installer.wxs"], cwd=stage, check=True)
            subprocess.run(["light", "-nologo", "-o", "product.msi", "Installer.wixobj"],
                           cwd=stage, check=True)
        (stage / "product.msi").replace(output)
    print(output)


if __name__ == "__main__":
    main()
