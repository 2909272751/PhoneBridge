"use strict";
// Deterministic vector tests for the pure-JS PBKDF2-HMAC-SHA256 fallback in
// app.js. Run with: node internal/app/web/frp_proof_test.js
//
// These tests validate the fallback against independent standards and against
// Node's own crypto module — they do not merely compare the fallback to
// itself, and they prove the WebCrypto path and the fallback produce the same
// proof.

const assert = require("assert");
const nodeCrypto = require("crypto");
const {
  deriveFRPProof,
  deriveFRPProofWebCrypto,
  deriveFRPProofFallback,
  pbkdf2SHA256,
  hmacSHA256,
  sha256,
  utf8Encode,
  base64urlFromArray,
  base64SaltBytes,
  webCryptoAvailable,
} = require("./app.js");

const hex = (bytes) => Buffer.from(bytes).toString("hex");

// ---------------------------------------------------------------------------
// 1. Independent standard vectors
// ---------------------------------------------------------------------------

// SHA-256 (FIPS 180-4 / RFC 6234) reference outputs.
function testSha256() {
  assert.strictEqual(hex(sha256(utf8Encode(""))), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  assert.strictEqual(hex(sha256(utf8Encode("abc"))), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  // Multi-block message (128 bytes -> crosses the 64-byte block boundary).
  assert.strictEqual(
    hex(sha256(utf8Encode("abcdefghbcdefghicdefghijdefghijkefghijklfghijklmghijklmnhijklmnoijklmnopjklmnopqklmnopqrlmnopqrsmnopqrstnopqrstu"))),
    "cf5b16a778af8380036ce59e7b0492370b249b11e8f07a51afac45037afee9d1",
  );
  console.log("  ok  SHA-256 standard vectors");
}

// HMAC-SHA256 (RFC 4231) test case 1.
function testHmac() {
  const key = new Uint8Array(20).fill(0x0b);
  const data = utf8Encode("Hi There");
  assert.strictEqual(hex(hmacSHA256(key, data)), "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7");
  console.log("  ok  HMAC-SHA256 RFC 4231 vector");
}

// PBKDF2-HMAC-SHA256 (RFC 7677) reference outputs.
async function testPbkdf2() {
  const password = utf8Encode("password");
  const salt = utf8Encode("salt");
  assert.strictEqual(
    hex(await pbkdf2SHA256(password, salt, 1, 32)),
    "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b",
  );
  assert.strictEqual(
    hex(await pbkdf2SHA256(password, salt, 4096, 32)),
    "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a",
  );
  console.log("  ok  PBKDF2-HMAC-SHA256 RFC 7677 vectors (iterations 1 and 4096)");
}

// ---------------------------------------------------------------------------
// 2. Cross-check the pure-JS primitives against Node's crypto module
// ---------------------------------------------------------------------------

async function testAgainstNodeCrypto() {
  const vectors = [
    { password: "密码🔐pass", salt: "salt-盐值", iterations: 1, dkLen: 32 },
    { password: "p@ssw0rd", salt: "salt-value-42", iterations: 1000, dkLen: 32 },
    { password: "Unicode用户名🙂", salt: "ch", iterations: 4096, dkLen: 32 },
    // dkLen beyond a single SHA-256 block (32 bytes) exercises the multi-block path.
    { password: "password", salt: "salt", iterations: 4096, dkLen: 40 },
  ];
  for (const v of vectors) {
    const expected = nodeCrypto
      .pbkdf2Sync(v.password, Buffer.from(v.salt, "utf8"), v.iterations, v.dkLen, "sha256")
      .toString("hex");
    const actual = hex(await pbkdf2SHA256(utf8Encode(v.password), utf8Encode(v.salt), v.iterations, v.dkLen, 500));
    assert.strictEqual(actual, expected, `PBKDF2 mismatch for iterations=${v.iterations}`);
  }
  console.log("  ok  PBKDF2 fallback matches Node crypto (unicode, multiple iterations)");
}

// ---------------------------------------------------------------------------
// 3. The full proof: WebCrypto path, fallback and Node crypto all agree
// ---------------------------------------------------------------------------

// decodeSalt independently turns a base64/base64url challenge salt into raw
// bytes (mirroring the real 88FRP console-login.js bytes(challenge.salt)) so
// the expected proof does not depend on app.js's own decoder.
function decodeSalt(challenge) {
  const cleaned = challenge.replace(/\s+/g, "").replace(/-/g, "+").replace(/_/g, "/");
  const padded = cleaned + "=".repeat((4 - (cleaned.length % 4)) % 4);
  return Buffer.from(padded, "base64");
}

function nodeProof(password, username, challenge, iterations, nonce) {
  const key = nodeCrypto.pbkdf2Sync(password, decodeSalt(challenge), iterations, 32, "sha256");
  const mac = nodeCrypto
    .createHmac("sha256", key)
    .update(`${nonce}\n${username.trim()}`, "utf8")
    .digest("base64")
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
  return mac;
}

async function testProofConsistency() {
  // challenges are base64/base64url encodings of the raw byte salts:
  // "c2FsdC12YWx1ZS00Mg==" -> "salt-value-42" (standard base64)
  // "5a-G56CB8J-UkA"       -> "密码🔐"        (base64url, unicode bytes)
  // "-__-AAEC"             -> [0xfb,0xff,0xfe,0,1,2] (base64url, non-ASCII bytes)
  const vectors = [
    { password: "correct-password", username: "alice", challenge: "c2FsdC12YWx1ZS00Mg==", iterations: 1000, nonce: "nonce-abc-987" },
    { password: "密码🔐", username: "用户 名", challenge: "5a-G56CB8J-UkA", iterations: 1, nonce: "n-1" },
    { password: "p@ss w0rd!", username: "bob-01", challenge: "-__-AAEC", iterations: 4096, nonce: "n-2" },
  ];
  for (const v of vectors) {
    const expected = nodeProof(v.password, v.username, v.challenge, v.iterations, v.nonce);
    const webCrypto = await deriveFRPProofWebCrypto(v.password, v.username, v.challenge, v.iterations, v.nonce);
    const fallback = await deriveFRPProofFallback(v.password, v.username, v.challenge, v.iterations, v.nonce, 100);
    const dispatcher = await deriveFRPProof(v.password, v.username, v.challenge, v.iterations, v.nonce);
    assert.strictEqual(webCrypto, expected, `WebCrypto proof mismatch (iterations=${v.iterations})`);
    assert.strictEqual(fallback, expected, `fallback proof mismatch (iterations=${v.iterations})`);
    assert.strictEqual(fallback, webCrypto, `WebCrypto vs fallback disagree (iterations=${v.iterations})`);
    assert.strictEqual(dispatcher, fallback, `deriveFRPProof did not reach the same proof`);
    // base64url: no padding and only URL-safe characters.
    assert.ok(!/[+/=]/.test(fallback), `proof is not base64url: ${fallback}`);
  }
  console.log("  ok  WebCrypto, fallback and Node crypto agree (unicode, multiple iterations)");
}

// ---------------------------------------------------------------------------
// 4. base64url and UTF-8 helpers
// ---------------------------------------------------------------------------

function testEncoding() {
  assert.strictEqual(base64urlFromArray(new Uint8Array([0xfb, 0xff, 0xfe])), "-__-");
  assert.strictEqual(hex(utf8Encode("A")), "41");
  assert.strictEqual(hex(utf8Encode("€")), "e282ac"); // U+20AC -> 3 bytes
  assert.strictEqual(hex(utf8Encode("😀")), "f09f9880"); // U+1F600 -> 4 bytes (surrogate pair)
  console.log("  ok  UTF-8 (incl. surrogate pairs) and base64url helpers");
}

function testBase64Salt() {
  assert.strictEqual(hex(base64SaltBytes("aGVsbG8=")), "68656c6c6f"); // "hello", standard base64
  assert.strictEqual(hex(base64SaltBytes("aGVsbG8")), "68656c6c6f"); // raw base64, no padding
  assert.strictEqual(hex(base64SaltBytes("-__-AAEC")), "fbfffe000102"); // base64url, non-ASCII bytes
  assert.strictEqual(hex(base64SaltBytes(" c2FsdA== ")), "73616c74"); // "salt", surrounding whitespace trimmed
  // Undecodable salts must throw, never fall back to UTF-8.
  assert.throws(() => base64SaltBytes("not-base64!!!"));
  assert.throws(() => base64SaltBytes("invalid*salt"));
  assert.throws(() => base64SaltBytes("!!!!"));
  console.log("  ok  base64SaltBytes decodes base64/base64url and rejects invalid salt");
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

(async () => {
  console.log("frp_proof_test.js");
  testSha256();
  testHmac();
  await testPbkdf2();
  await testAgainstNodeCrypto();
  await testProofConsistency();
  testEncoding();
  testBase64Salt();
  console.log(`  webCryptoAvailable in this Node runtime: ${webCryptoAvailable()}`);
  console.log("All frp proof tests passed.");
})().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
