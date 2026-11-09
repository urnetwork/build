ur, the URnetwork sidecar, for Windows

This zip holds:

  ur.exe             the ur command
  git-remote-ur.exe  a copy of ur.exe, which git runs for ur:// remotes
  LICENSE            the Mozilla Public License 2.0, which ur is under
  NOTICE             the notices of the code ur includes

To install, put both .exe files in

  %LOCALAPPDATA%\Programs\URnetwork\

and add that folder to your PATH, so that you and git can run them:

  1. Open Settings, search for "environment variables", and choose
     "Edit environment variables for your account".
  2. Select Path, choose Edit, then New, and enter
     %LOCALAPPDATA%\Programs\URnetwork
  3. Open a new terminal and run: ur --version

Keep git-remote-ur.exe beside ur.exe: git finds the helper on PATH by its
name, and ur acts as the helper when it runs under that name.

Both files are signed. To check a signature, right-click the file, choose
Properties, then Digital Signatures.

ur does not update itself. To update, replace both files with those of a
newer release.
