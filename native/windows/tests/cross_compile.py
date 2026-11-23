#!/usr/bin/env python3
"""Use existing clang-cl/LLD and an existing xwin SDK cache; never downloads or installs.
The result is build evidence only. It is not a Windows/GPU runtime test.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--sdk', type=Path, default=Path.home() / 'Library/Caches/cargo-xwin/xwin')
    parser.add_argument('--clang-cl', default=shutil.which('clang-cl') or '/opt/homebrew/opt/llvm/bin/clang-cl')
    parser.add_argument('--lld-link', default=shutil.which('lld-link') or '/opt/homebrew/opt/llvm/bin/lld-link')
    parser.add_argument('--output', type=Path, default=ROOT / 'evidence/cross')
    args = parser.parse_args()
    for executable in (args.clang_cl, args.lld_link):
        if not Path(executable).is_file():
            raise SystemExit(f'Existing tool is missing: {executable}; no installation was attempted')
    includes = [args.sdk / 'crt/include'] + [args.sdk / 'sdk/include' / n for n in ('ucrt', 'shared', 'um', 'winrt')]
    libs = [args.sdk / 'crt/lib/x86_64', args.sdk / 'sdk/lib/ucrt/x86_64', args.sdk / 'sdk/lib/um/x86_64']
    for directory in includes + libs:
        if not directory.is_dir():
            raise SystemExit(f'Existing SDK directory is missing: {directory}')
    args.output.mkdir(parents=True, exist_ok=True)
    steps = []

    def run(command):
        result = subprocess.run(command, cwd=ROOT, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        steps.append({'command': list(map(str, command)), 'exitCode': result.returncode, 'output': result.stdout})
        print(result.stdout, end='')
        (args.output / 'build-log.json').write_text(json.dumps(steps, indent=2) + '\n')
        if result.returncode:
            raise SystemExit(result.returncode)
        return result.stdout

    inputs = {name: hashlib.sha256((ROOT / name).read_bytes()).hexdigest()
              for name in ('main.cpp', 'protocol_core.h', 'decoder_fixture.h', 'helper.manifest')}
    run([args.clang_cl, '--version'])
    common = [args.clang_cl, '--target=x86_64-pc-windows-msvc', '/std:c++17', '/EHsc', '/MT', '/O2', '/W4',
              '/permissive-', '/utf-8', '/guard:cf', '/D_WIN32_WINNT=0x0A00', '/DWINVER=0x0A00',
              '/DUNICODE', '/D_UNICODE', '/D_ALLOW_COMPILER_AND_STL_VERSION_MISMATCH']
    # Cached MSVC STL is newer than the installed Clang. This flag only bypasses its compiler-version gate.
    # Windows CI uses current MSVC and does not use this override.
    common += ['/imsvc' + str(p) for p in includes]
    obj = args.output / 'main.obj'
    executable = args.output / 'vantry-remote-helper.exe'
    run(common + ['/c', '/Fo' + str(obj), '--', str(ROOT / 'main.cpp')])
    system_libraries = ['d3d11', 'dxgi', 'mfplat', 'mf', 'mfuuid', 'wmcodecdspuuid', 'ole32', 'oleaut32', 'uuid',
                        'strmiids', 'wtsapi32', 'user32', 'kernel32', 'advapi32']
    run([args.lld_link, '/machine:x64', '/subsystem:console', '/guard:cf', '/dynamicbase', '/nxcompat',
         '/manifest:embed', '/manifestuac:no', '/manifestinput:' + str(ROOT / 'helper.manifest'), '/out:' + str(executable), os.path.relpath(obj, ROOT)] +
        ['/libpath:' + str(p) for p in libs] + [n + '.lib' for n in system_libraries])
    data = executable.read_bytes()
    offset = int.from_bytes(data[0x3c:0x40], 'little')
    if data[:2] != b'MZ' or data[offset:offset + 4] != b'PE\0\0' or data[offset + 4:offset + 6] != b'\x64\x86':
        raise SystemExit('Unexpected executable format')
    start = data.find(b'<?xml')
    end = data.find(b'</assembly>', start) + len(b'</assembly>')
    embedded = data[start:end]
    manifest = ET.fromstring(embedded)
    request = manifest.find('.//{urn:schemas-microsoft-com:asm.v3}requestedExecutionLevel')
    assert request is not None and request.attrib == {'level': 'asInvoker', 'uiAccess': 'false'}, 'Embedded UAC manifest was changed by the linker'
    assert manifest.find('.//{http://schemas.microsoft.com/SMI/2016/WindowsSettings}dpiAwareness').text == 'PerMonitorV2, PerMonitor'
    (args.output / 'embedded.manifest').write_bytes(embedded)
    summary = {'status': 'CROSS_BUILD_PASS', 'architecture': 'AMD64', 'bytes': len(data),
               'sha256': hashlib.sha256(data).hexdigest(), 'executable': str(executable),
               'inputSHA256': inputs, 'embeddedUACAndDPINamespacesVerified': True,
               'runtimeExecuted': False, 'hardwareVerifiedOnThisMachine': False}
    (args.output / 'summary.json').write_text(json.dumps(summary, indent=2) + '\n')
    print(json.dumps(summary, indent=2))


if __name__ == '__main__':
    main()
