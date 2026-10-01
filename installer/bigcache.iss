; Inno Setup script for the BIGCache Windows installer.
; Built by .github/workflows/release.yml:  ISCC.exe /DAppVersion=1.2.3 installer\bigcache.iss

#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif
#ifndef SourceDir
  #define SourceDir "..\dist\windows-amd64"
#endif

[Setup]
AppId={{7C1E0E6A-5C2B-4C0E-9C6F-BIGCACHE0001}
AppName=BIGCache
AppVersion={#AppVersion}
AppVerName=BIGCache {#AppVersion}
AppPublisher=XTRBIG
AppPublisherURL=https://github.com/XTRBIG/BIGCache
AppSupportURL=https://github.com/XTRBIG/BIGCache/issues
DefaultDirName={autopf}\BIGCache
DefaultGroupName=BIGCache
DisableProgramGroupPage=yes
OutputDir=..\dist
OutputBaseFilename=BIGCache-Setup-{#AppVersion}-windows-x64
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
PrivilegesRequired=admin
Compression=lzma2
SolidCompression=yes
LicenseFile=..\LICENSE
InfoBeforeFile=before.txt
InfoAfterFile=after.txt
ChangesEnvironment=yes
UninstallDisplayIcon={app}\bigcache.exe
WizardStyle=modern

[Tasks]
Name: "service"; Description: "Register the BIGCache Windows service (automatic start after you edit the configuration)"; GroupDescription: "Service:"
Name: "path"; Description: "Add BIGCache to the system PATH"; GroupDescription: "Environment:"

[Files]
Source: "{#SourceDir}\bigcache.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\README.md"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\LICENSE"; DestDir: "{app}"; DestName: "LICENSE.txt"; Flags: ignoreversion
Source: "..\examples\config.windows.json"; DestDir: "{app}"; DestName: "config.example.json"; Flags: ignoreversion
Source: "..\examples\config.windows.json"; DestDir: "{commonappdata}\BIGCache"; DestName: "config.json"; Flags: onlyifdoesntexist uninsneveruninstall

[Dirs]
Name: "{commonappdata}\BIGCache"

[Icons]
Name: "{group}\Edit BIGCache configuration"; Filename: "notepad.exe"; Parameters: """{commonappdata}\BIGCache\config.json"""
Name: "{group}\BIGCache statistics"; Filename: "{cmd}"; Parameters: "/k ""{app}\bigcache.exe"" stats --watch 2s"
Name: "{group}\BIGCache log"; Filename: "notepad.exe"; Parameters: """{commonappdata}\BIGCache\bigcache.log"""
Name: "{group}\BIGCache README"; Filename: "{app}\README.md"
Name: "{group}\Uninstall BIGCache"; Filename: "{uninstallexe}"

[Run]
Filename: "{app}\bigcache.exe"; Parameters: "service install -c ""{commonappdata}\BIGCache\config.json"""; StatusMsg: "Registering the BIGCache service..."; Flags: runhidden; Tasks: service
Filename: "notepad.exe"; Parameters: """{commonappdata}\BIGCache\config.json"""; Description: "Edit the BIGCache configuration now"; Flags: postinstall nowait skipifsilent shellexec

[Registry]
Root: HKLM; Subkey: "SYSTEM\CurrentControlSet\Control\Session Manager\Environment"; ValueType: expandsz; ValueName: "Path"; ValueData: "{olddata};{app}"; Tasks: path; Check: NeedsAddPath(ExpandConstant('{app}'))

[Code]
const
  EnvKey = 'SYSTEM\CurrentControlSet\Control\Session Manager\Environment';

function NeedsAddPath(Param: string): Boolean;
var
  OrigPath: string;
begin
  if not RegQueryStringValue(HKEY_LOCAL_MACHINE, EnvKey, 'Path', OrigPath) then
  begin
    Result := True;
    exit;
  end;
  Result := Pos(';' + Uppercase(Param) + ';', ';' + Uppercase(OrigPath) + ';') = 0;
end;

procedure RemoveFromPath(Dir: string);
var
  OrigPath, NewPath: string;
  P: Integer;
begin
  if not RegQueryStringValue(HKEY_LOCAL_MACHINE, EnvKey, 'Path', OrigPath) then
    exit;
  NewPath := ';' + OrigPath + ';';
  P := Pos(';' + Uppercase(Dir) + ';', Uppercase(NewPath));
  if P = 0 then
    exit;
  Delete(NewPath, P, Length(Dir) + 1);
  NewPath := Copy(NewPath, 2, Length(NewPath) - 2);
  RegWriteExpandStringValue(HKEY_LOCAL_MACHINE, EnvKey, 'Path', NewPath);
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usPostUninstall then
    RemoveFromPath(ExpandConstant('{app}'));
end;

// Before any file is removed: detach the cached disks, stop the service
// (which writes dirty blocks back to the HDDs), verify that nothing is left
// unwritten and deregister the service. The uninstall is aborted if that
// fails, so data on the cache is never orphaned by removing the program.
function InitializeUninstall(): Boolean;
var
  Params, Cfg: string;
  ResultCode: Integer;
begin
  Result := True;
  Cfg := ExpandConstant('{commonappdata}\BIGCache\config.json');
  Params := 'teardown -c "' + Cfg + '"';
  if MsgBox('Delete the cache file (or wipe the cache partition) named in the configuration?' + #13#10 +
            'It is only removed after all cached writes have reached the HDDs.',
            mbConfirmation, MB_YESNO) = IDYES then
    Params := Params + ' --purge-cache';
  if MsgBox('Delete the configuration and log in ' + ExpandConstant('{commonappdata}\BIGCache') + ' as well?',
            mbConfirmation, MB_YESNO) = IDYES then
    Params := Params + ' --purge-config';
  if not Exec(ExpandConstant('{app}\bigcache.exe'), Params, '', SW_SHOW, ewWaitUntilTerminated, ResultCode) then
  begin
    MsgBox('Could not run bigcache.exe teardown. Stop the BIGCache service manually and run the uninstaller again.', mbError, MB_OK);
    Result := False;
    exit;
  end;
  if ResultCode <> 0 then
  begin
    MsgBox('BIGCache could not be shut down cleanly (exit code ' + IntToStr(ResultCode) + ').' + #13#10 +
           'Cached writes may not have reached the HDDs yet. Fix the problem reported in the console window ' +
           '(or run "bigcache teardown --force" as Administrator to discard them), then run the uninstaller again.',
           mbError, MB_OK);
    Result := False;
  end;
end;
