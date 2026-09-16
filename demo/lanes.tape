# vhs tape for the bowt lanes demo → docs/demo.gif
# Regenerate with: demo/record.sh  (seeds an isolated demo, then runs vhs)
Output docs/demo.gif

Set Shell "bash"
Set FontSize 18
Set Width 1200
Set Height 720
Set Padding 22
Set Theme "Catppuccin Mocha"
Set TypingSpeed 55ms

# Setup (hidden): point HOME at the seeded demo, use the demo binary, clean prompt.
Hide
Type "export HOME=/tmp/bowt-demo PATH=/tmp/bowt-demo/bin:$PATH PS1='$ '"
Enter
Type "cd /tmp/bowt-demo/acme-worktrees/feat-checkout && clear"
Enter
Show

Sleep 600ms
Type "# three tasks, three isolated worktrees (own port, DB, env)"
Enter
Type "bowt ls"
Sleep 400ms
Enter
Sleep 2s

Type "# dispatch an agent to plan the checkout work"
Enter
Type "bowt spawn --agent demo"
Sleep 400ms
Enter
Sleep 3s

Type "# the fleet — every lane and where it stands"
Enter
Type "bowt lanes"
Sleep 400ms
Enter
Sleep 2500ms

Type "# per-worktree cockpit: branch x lane x gate"
Enter
Type "bowt status"
Sleep 400ms
Enter
Sleep 3500ms
