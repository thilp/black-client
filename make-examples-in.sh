#!/bin/sh

set -e

create_in() {
    mkdir -p "$1"

    EMPTY_FILE="$1/empty.py"
    QUOTES="$1/quotes.py"
    INVALID="$1/invalid.py"
    BROKEN_LINK="$1/broken-link.py"
    SPECIAL_LINK="$1/special-link.py"

    echo ""             >"$EMPTY_FILE"
    echo "print('x')"   >"$QUOTES"
    echo "print("       >"$INVALID"
    ln -s xyz "$BROKEN_LINK"
    ln -s /dev/null "$SPECIAL_LINK"

    UNREADABLE="$1/unreadable.py"
    cp "$QUOTES" "$UNREADABLE"
    chmod a-r "$UNREADABLE"

    EMPTY_DIR="$1/empty_dir"
    mkdir "$EMPTY_DIR"

    for x in $(ls "$1"); do
        ln -s "$x" "$1/link-to-$(basename "$x")"
    done
}

DIR_WITH_ALL="$1/dir-with-all"
mkdir "$DIR_WITH_ALL"

create_in "$DIR_WITH_ALL"
create_in "$1"
