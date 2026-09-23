-- Copyright 2026 The zb Authors
-- SPDX-License-Identifier: MIT

local shellMetatable <const> = {}

local function shell(env)
  return setmetatable(env, shellMetatable)
end

-- `zb build` calls the __outputs metamethod
-- with the system triple string that it is attempting to build for.
function shellMetatable:__outputs(system)
  -- Create a copy of the env table to pass to the built-in derivation function,
  -- since we want to modify it for the current system.
  local args = {}
  for k, v in pairs(self) do
    if k ~= "shCommand" and k ~= "powershellCommand" then
      -- The defaultOutput built-in will either use the __outputs metamethod
      -- or return its first argument, as appropriate.
      -- This allows us to resolve any dependencies for the current system.
      args[k] = defaultOutput(v, system)
    end
  end

  args.system = system
  if system:match(".*windows.*") then
    args.builder = [[C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe]]
    args.args = { "-Command", self.powershellCommand }
  else
    args.builder = "/bin/sh"
    args.args = { "-c", self.shCommand }
  end
  return derivation(args)
end

-- You can build this with `zb build demo.lua#hello`.
hello = shell {
  name = "hello.txt";
  ["in"] = path "hello.txt";
  shCommand = "echo what; while read line; do echo \"$line\"; done < $in > $out";
  powershellCommand = "Copy-Item ${env:in} ${env:out}";
}

-- You can build this with `zb build demo.lua#multistep`.
multistep = shell {
  name = "hello2.txt";
  ["in"] = hello;
  shCommand = "\z
    while read line; do\n\z
      echo \"$line\"\n\z
    done < $in > $out\n\z
    while read line; do\n\z
      echo \"$line\"\n\z
    done < $in >> $out";
  powershellCommand = [[$x = Get-Content -Raw ${env:in} ; ($x + $x) | Out-File -NoNewline -Encoding ascii -FilePath ${env:out}]];
}
