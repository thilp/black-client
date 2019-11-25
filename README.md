# black-client

## Why

[black]: https://github.com/psf/black
[blackd]: https://black.readthedocs.io/en/stable/blackd.html

[Black][] is great and fast but when you invoke it repeatedly, you spend some
time firing up the same Python process again and again.
You could avoid wasting that time if the same Black process was somehow already
running every time you need it.
That is why Black also provides **[blackd][]**, a server acting like an
always-running `black` process.

[kopf]: https://github.com/zalando-incubator/kopf

blackd comes without an official client, so you have to either find one or
write your own.
This repository is my attempt at a Black client interacting with blackd.

## Results

Compared to `black`, without any modification to blackd (e.g. I didn't try
running it on Pypy), that client is usually:

- **faster** for only one or a few (< ~8) files to format/check at a time
  (e.g. ~30% of `black`'s run time for 4 files);
- **slower** for more
  (e.g. ~130% of `black`'s run time for 160 Python files — the whole git
  repository of [kopf][]). 

## Analysis

My take on these results is that the current design of the **blackd protocol
is inefficient** for the use-cases I tried.

Reducing the source analysis time would mean reducing it in blackd,
which shares that part with Black, so Black would likely get the same
reduction.
So source analysis time is irrelevant in our context, and we can concentrate on:

1. start-up time,
1. identification of files to analyze,
1. overwriting of files to format (without `--check`),
1. reporting.

A Go-based solution (or really anything faster than Python) is likely to
perform as well or better than the Python-based Black on all these phases.
However, the current blackd protocol requires `black-client` to **send the
contents** of the files to analyze **over the network**, with **one HTTP
request** per file.

This fits the use-case of having blackd running on a remote host, but I doubt
anyone does this (consider for example that blackd doesn't support encryption,
which would anyway slow the process even more).

For the use-case where blackd runs locally, sending file contents over separate
HTTP requests creates a lot of IO waste:

- you have to manage HTTP connections;
- you have to read the file's contents (from the filesystem),
  and blackd will do it as well (from the network);
- blackd will write the reformatted contents (or the diff) to the network,
  and you have to read it and then write it in the filesystem.

## Beyond

### Make blackd work more

If blackd is responsible for reading and writing source files in the filesystem
the contents of these files no longer need to be serialized and deserialized
over the network.

### Don't use HTTP

As far as I can tell, blackd only uses HTTP as a simple way to transmit a
string of bytes (from client to server or bidirectionally), short key-value
string pairs (from client to server), and an integer (from server to client, as
status code).

[cbor]: https://en.wikipedia.org/wiki/CBOR

Given the assumption that blackd will run on the same host as the client, it
seems safe to go instead with a single Unix socket (per client × blackd pair)
and use something like [CBOR][] for message passing.

If blackd doesn't read or write from the filesystem (i.e. doesn't do any I/O
beyond networking), then using a single socket should not increase application
complexity because there is no need for a given blackd instance to accept
multiple simultaneous requests. 
Indeed, the processing of each request being CPU-bound, there would be no
reason to interrupt one to accept another, since there is nothing to wait for.

Otherwise, it should be possible to simply use two sockets (one for reading,
the other for writing) and message IDs.
