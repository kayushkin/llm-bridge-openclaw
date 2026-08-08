"""Sabotage-score the rune-boundary truncation tests.

A suite that passes tells you nothing on its own. This breaks the fix in eight
ways and checks that the tests notice six of them and, just as importantly, do
NOT notice the other two. A scorer with no known-negative control reports CAUGHT
for everything and looks perfect while measuring nothing.

What this copy adds over the four sibling harnesses in the sweep:

  * This repo has TWO call sites with TWO budgets, so there are two call-site
    rows rather than one. The interesting claim is not that each is caught, it
    is that each is caught by its OWN test and leaves the other green. That is
    a coverage claim about a suite, which is the kind of sentence earlier passes
    got wrong by asserting it in a commit message instead of measuring it, so
    every row here reports the exact set of tests that went red.

  * One row genuinely panics in production code (the negative-budget guard).
    A verdict the table can reach is stronger evidence than the same verdict
    reached only by feeding synthetic output to the classifier, so the panic
    branch here is exercised by a real mutation as well as by self_test().

Three things inherited from earlier passes of this sweep, all learned expensively:

  * Mutations are written as drifted comparisons or substitutions that keep every
    identifier live. Deleting the walk-back orphans the `utf8` import, and
    `go test` runs vet, so the case would report a compile error instead of a
    score -- and a compile error hides whether any test would have caught the
    behaviour.

  * CAUGHT is split into assertion-fired and guard-fired. A test can go red
    because its own fixture blew up (t.Fatalf on a setup step) rather than
    because it detected the defect. That is not coverage, and counting it as
    coverage inflates the score.

  * classify() is exercised against every verdict it can return, including the
    fixture-panic verdict no row in this table produces. A table cannot score its
    own scorer: the forty-eighth pass shipped a scorer whose panic branch was
    unreachable and read 5/5 with it dead, because nothing it mutated panicked.

Run from anywhere:  python3 scripts/sabotage-truncation.py
"""

import pathlib
import re
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent

WALKBACK = "for cut > 0 && !utf8.RuneStart(s[cut]) {"
CLAMP = "\tif maxBytes < 0 {"
FITS = "\tif len(s) <= maxBytes {"

# The two call sites. Each is replaced by an inline func literal reintroducing
# the original byte cut, rather than by deleting the call: the helper stays
# referenced from the other file either way, but composing keeps the mutation
# a behaviour change rather than a compile experiment.
TRANSLATE_CALL = "\t\t\t\t\tOutput: truncateAtRuneBoundaryWithEllipsis(block.Text, 500),"
TRANSLATE_BYTECUT = (
    "\t\t\t\t\tOutput: func(s string) string {\n"
    "\t\t\t\t\t\tif len(s) <= 500 {\n"
    "\t\t\t\t\t\t\treturn s\n"
    "\t\t\t\t\t\t}\n"
    "\t\t\t\t\t\treturn s[:500] + \"...\"\n"
    "\t\t\t\t\t}(block.Text),"
)

CLIENT_CALL = ('\t\treturn fmt.Errorf("http %d: %s", resp.StatusCode, '
               'truncateAtRuneBoundaryWithEllipsis(string(body), 200))')
CLIENT_BYTECUT = (
    "\t\tbyteCut := func(s string) string {\n"
    "\t\t\tif len(s) <= 200 {\n"
    "\t\t\t\treturn s\n"
    "\t\t\t}\n"
    "\t\t\treturn s[:200] + \"...\"\n"
    "\t\t}\n"
    '\t\treturn fmt.Errorf("http %d: %s", resp.StatusCode, byteCut(string(body)))'
)

CASES = [
    # (label, file, old, new, expect_caught)
    ("the walk-back never runs, so the cut splits a rune again",
     "client.go", WALKBACK, "for cut > len(s) && !utf8.RuneStart(s[cut]) {", True),
    ("the helper trims to nothing instead of to the boundary",
     "client.go", '\treturn s[:cut] + "..."', '\treturn s[:cut*0] + "..."', True),
    ("the walk-back stops one byte short, leaving the rune split",
     "client.go", '\treturn s[:cut] + "..."', '\treturn s[:cut+1] + "..."', True),
    # Reaches the panic verdict for real, rather than only through self_test().
    ("the negative-budget clamp stops clamping, so s[:-1] panics again",
     "client.go", CLAMP, "\tif maxBytes < -1 {", True),
    # The call sites, not the helper. The helper being correct does not prove a
    # caller uses it, and these are the two mutations that say so. Each should
    # be caught by its own call-site test ALONE -- reported, not assumed.
    ("only the tier-1 translate.go call site reverts to a plain byte cut",
     "translate.go", TRANSLATE_CALL, TRANSLATE_BYTECUT, True),
    ("only the client.go error-string call site reverts to a plain byte cut",
     "client.go", CLIENT_CALL, CLIENT_BYTECUT, True),
    # Known-NEGATIVE controls. Neither changes behaviour, so a harness that
    # reports CAUGHT here is reporting CAUGHT for everything.
    #
    # maxBytes==0 clamps to 0, which it already was, so widening the clamp to
    # cover it is a no-op.
    ("CONTROL (no-op): the negative-budget clamp widens from <0 to <=0",
     "client.go", CLAMP, "\tif maxBytes <= 0 {", False),
    # A pure rewrite of the same comparison. If this scores CAUGHT the suite is
    # sensitive to something other than behaviour.
    ("CONTROL (no-op): the fits-in-budget test is rewritten, same semantics",
     "client.go", FITS, "\tif len(s) < maxBytes+1 {", False),
]

TESTS = ("TestTruncateAtRuneBoundaryWithEllipsisSlidesTheCutAcrossEveryOffset|"
         "TestTruncateAtRuneBoundaryWithEllipsisMixedWidths|"
         "TestTruncateAtRuneBoundaryWithEllipsisEdgeCases|"
         "TestTruncateAtRuneBoundaryWithEllipsisLeavesTheBudgetArithmeticAlone|"
         "TestToolResultOutputStaysValidUTF8|"
         "TestHTTPErrorBodyStaysValidUTF8")

# Messages from fixture guards rather than from an assertion about truncation.
# A red run that shows only these is the test falling over, not detecting.
GUARD_MARKERS = (
    "marshal content:",
    "marshal event:",
    "marshal error message:",
    "want 1 tool-result event",
    "event carries no ToolResult",
    "want an error from a 500 response",
    "the cut never landed inside a rune",
    "the known-negative control never ran",
)

FAIL_LINE = re.compile(r"^\s*truncate_test\.go:\d+: (.*)$", re.M)
# Top-level test names that went red, e.g. "--- FAIL: TestFoo (0.00s)". Subtests
# are indented, so the anchor at column zero keeps this to the parents.
FAIL_TEST = re.compile(r"^--- FAIL: (\w+)", re.M)
# A stack frame naming a .go file, e.g. "\t/home/u/repo/client.go:118". The full
# path is captured because the Go runtime's own frames (runtime/panic.go) would
# otherwise pass a bare-filename filter and be mistaken for our source.
FRAME = re.compile(r"^\s+(/\S+\.go):(\d+)", re.M)


def classify(output):
    """Return (verdict, detail) for one sabotage run's output."""
    if "[build failed]" in output or "declared and not used" in output:
        return "COMPILE ERROR", "mutation orphaned an identifier -- rewrite it"
    if "panic:" in output:
        # Read WHERE the panic is, not just that there is one. A mutation that
        # crashes production code the test drove it into IS detection -- the
        # program died instead of returning a wrong answer, and the test names
        # the input that did it. A panic in the fixture is the test falling
        # over before it asserted anything, which is not coverage.
        #
        # Pick the first non-test frame rather than requiring that no test frame
        # is present: a panic inside production code always has the calling test
        # frame below it, so an "and no _test.go frame" clause is unreachable and
        # would file every real crash as fixture damage.
        frames = [f for f, _ in FRAME.findall(output.split("panic:", 1)[1])
                  if f.startswith(str(REPO) + "/")]
        source = next((f for f in frames if not f.endswith("_test.go")), None)
        if source:
            return ("CAUGHT (panic in %s)" % pathlib.Path(source).name,
                    "test drove the mutation into a crash")
        return "CAUGHT (panic in fixture -- NOT coverage)", "the test fell over before asserting"
    if "--- FAIL" not in output:
        return "UNNOTICED", ""
    messages = FAIL_LINE.findall(output)
    if not messages:
        return "CAUGHT (no assertion text -- NOT coverage)", ""
    guard = [m for m in messages if any(g in m for g in GUARD_MARKERS)]
    real = [m for m in messages if m not in guard]
    if not real:
        return "CAUGHT (guard only -- NOT coverage)", guard[0][:90]
    return "CAUGHT", real[0][:90]


def self_test():
    """Exercise classify() directly against every verdict it can return.

    The forty-third pass's rule: when you automate a check, the check is the next
    unmeasured claim. A classify() that can never return "guard only" prints the
    clean score you were hoping for. Rather than trust that these branches are
    reachable, drive all six from synthetic output. The production-code panic
    verdict IS reachable from the table here, and is probed anyway -- the
    fixture-panic verdict is the only one that is not.
    """
    probes = [
        ("--- FAIL: X\n    truncate_test.go:54: result is not valid UTF-8\n", "CAUGHT"),
        ("--- FAIL: X\n    truncate_test.go:99: want 1 tool-result event, got 0\n",
         "CAUGHT (guard only -- NOT coverage)"),
        ("ok  \tgithub.com/x\n", "UNNOTICED"),
        # Both panic branches, distinguished only by which file the top repo
        # frame names. The runtime's own panic.go frame is present in each and
        # must not be mistaken for our source. The production-code probe carries
        # a test frame too, because a real one always does.
        ("panic: slice bounds out of range\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/client.go:118\n"
         "\t%s/truncate_test.go:155\n" % (REPO, REPO), "CAUGHT (panic in client.go)"),
        ("panic: slice bounds out of range\n"
         "\t/usr/lib/go/src/runtime/panic.go:860 +0x13a\n"
         "\t%s/truncate_test.go:200\n" % REPO,
         "CAUGHT (panic in fixture -- NOT coverage)"),
        ("# github.com/x [build failed]\n", "COMPILE ERROR"),
    ]
    ok = True
    for output, want in probes:
        got, _ = classify(output)
        if got != want:
            print(f"  classifier SELF-TEST FAIL: got {got!r}, want {want!r}")
            ok = False
    print(f"  classifier self-test: {'all 6 verdicts reachable' if ok else 'BROKEN'}")
    return ok


def restore():
    subprocess.run(["git", "checkout", "--", "client.go", "translate.go"],
                   cwd=REPO, check=True)


print("Sabotaging the rune-boundary truncation fix in llm-bridge-openclaw\n")
if not self_test():
    sys.exit(2)
print()

score = 0
for label, fname, old, new, expect in CASES:
    restore()
    p = REPO / fname
    text = p.read_text()
    # Exact string replacement, asserted to occur exactly once. A stale pattern
    # silently mutates nothing and scores a bogus UNNOTICED.
    if text.count(old) != 1:
        print(f"  SETUP FAIL   {label}\n      pattern appears {text.count(old)}x in {fname}, want 1")
        continue
    p.write_text(text.replace(old, new, 1))

    r = subprocess.run(["go", "test", "-count=1", "-run", TESTS, "."],
                       cwd=REPO, capture_output=True, text=True)
    out = r.stdout + r.stderr
    verdict, detail = classify(out)

    caught = verdict.startswith("CAUGHT") and "NOT coverage" not in verdict
    ok = caught == expect
    score += ok
    want = "CAUGHT" if expect else "UNNOTICED"
    print(f"  {'ok  ' if ok else 'BAD '} {verdict:<32} (want {want:<9}) {label}")
    if detail:
        print(f"         -> {detail}")
    # Which tests fired. For the two call-site rows this is the whole point:
    # the claim is that each is caught by its own test alone, and a claim in a
    # comment is an unmeasured claim.
    red = FAIL_TEST.findall(out)
    if red:
        print(f"         red: {', '.join(red)}")

restore()
print(f"\nscore {score}/{len(CASES)}")
sys.exit(0 if score == len(CASES) else 1)
