# The shell of a UMP-installed system: a colored prompt, completion and
# the usual aliases, the same on every system UMP installs. On Ubuntu and
# Debian it is ~/.bashrc (from /etc/skel/.bashrc, root's too); on NixOS it
# is part of UMP's module (/etc/bashrc). Yours to change.
case $- in *i*) ;; *) return ;; esac

HISTCONTROL=ignoreboth
HISTSIZE=10000
HISTFILESIZE=20000
shopt -s histappend checkwinsize
command -v lesspipe >/dev/null && eval "$(SHELL=/bin/sh lesspipe)"

# user@host in green (red for root), the directory in blue
if [ "$(id -u)" = 0 ]; then
	PS1='\[\e[1;31m\]\u\[\e[0m\]@\[\e[1;32m\]\h\[\e[0m\] \[\e[1;34m\]\w\[\e[0m\] # '
else
	PS1='\[\e[1;32m\]\u@\h\[\e[0m\] \[\e[1;34m\]\w\[\e[0m\] $ '
fi
case "$TERM" in xterm*|rxvt*|screen*|tmux*) PS1="\[\e]0;\u@\h: \w\a\]$PS1" ;; esac

if command -v dircolors >/dev/null; then
	eval "$(dircolors -b)"
	alias ls='ls --color=auto'
	alias grep='grep --color=auto'
	alias diff='diff --color=auto'
	alias ip='ip --color=auto'
fi
alias ll='ls -alF'
alias la='ls -A'
alias l='ls -CF'

if ! shopt -oq posix; then
	if [ -f /usr/share/bash-completion/bash_completion ]; then
		. /usr/share/bash-completion/bash_completion
	elif [ -f /etc/bash_completion ]; then
		. /etc/bash_completion
	fi
fi
[ -f ~/.bash_aliases ] && . ~/.bash_aliases
