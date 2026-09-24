# Firefox process-owner fixture

`firefox-process.mjs` is an unmodified offline test fixture from sibling `mmm`
commit `05776212aeb5c6029fa6f9e4f87abb10c0c614b3`, path
`ur.io/react/tests/firefox-process.mjs`. It lets the setup lifecycle regression
exercise the existing exact-profile cleanup without requiring another checkout
or starting any host processes. Production imports the sibling checkout's
module, not this fixture. Changes to that shared contract should update this
fixture and run its sibling process-owner tests as well.
