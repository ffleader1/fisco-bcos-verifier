// usage: node compile.js <contractsDir> <outDir>
const solc = require('solc'); const fs = require('fs'); const path = require('path');
const [dir, out] = process.argv.slice(2);
const sources = {};
for (const f of fs.readdirSync(dir).filter(f => f.endsWith('.sol'))) sources[f] = { content: fs.readFileSync(path.join(dir, f), 'utf8') };
const input = { language: 'Solidity', sources, settings: { optimizer: { enabled: true, runs: 200 }, evmVersion: 'constantinople',
  outputSelection: { '*': { '*': ['abi', 'evm.bytecode.object'] } } } };
const res = JSON.parse(solc.compile(JSON.stringify(input), { import: p => ({ contents: fs.readFileSync(path.join(dir, p), 'utf8') }) }));
let bad = false;
for (const e of res.errors || []) { console.error(e.formattedMessage); if (e.severity === 'error') bad = true; }
if (bad) process.exit(1);
for (const name of ['LoanLedger', 'AuditTrail']) {
  const c = res.contracts[name + '.sol'][name];
  fs.writeFileSync(path.join(out, name + '.abi'), JSON.stringify(c.abi));
  fs.writeFileSync(path.join(out, name + '.bin'), c.evm.bytecode.object);
  console.log(name, 'bin bytes', c.evm.bytecode.object.length / 2);
}
