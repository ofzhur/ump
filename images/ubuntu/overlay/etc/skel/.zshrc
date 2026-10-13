# ~/.zshrc of a UMP-installed system: oh-my-zsh from /usr/share/oh-my-zsh
# with the crunch theme. Yours to change; /etc/skel/.zshrc is the copy
# new users get. Updates are off (the copy is the system's, apt-free);
# `git -C /usr/share/oh-my-zsh pull` as root updates it for everyone.
export ZSH=/usr/share/oh-my-zsh
export ZSH_CACHE_DIR="$HOME/.cache/oh-my-zsh"
mkdir -p "$ZSH_CACHE_DIR/completions"
ZSH_THEME="crunch"
zstyle ':omz:update' mode disabled
DISABLE_AUTO_UPDATE=true
DISABLE_MAGIC_FUNCTIONS=true
HIST_STAMPS="yyyy-mm-dd"
plugins=(git sudo systemd)
source "$ZSH/oh-my-zsh.sh"

HISTSIZE=10000
SAVEHIST=20000
setopt HIST_IGNORE_SPACE HIST_IGNORE_ALL_DUPS SHARE_HISTORY
alias ll='ls -alF'
alias la='ls -A'
alias ip='ip --color=auto'
[ -f ~/.zsh_aliases ] && source ~/.zsh_aliases
