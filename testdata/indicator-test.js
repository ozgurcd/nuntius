const fs = require('node:fs'), vm = require('node:vm');
const script = fs.readFileSync('app.js', 'utf8');
const fn = script.slice(script.indexOf('function laneIndicator('), script.indexOf('\nfunction renderAgentTabs('));
const ctx = vm.createContext({}); vm.runInContext(fn, ctx);
const cases = [
 [{state:'idle'},'idle'], [{state:'running'},'running'],
 [{state:'ended'},'finished'], [{state:'completed'},'finished'],
 [{state:'running',display_state:'stuck?'},'stuck'],
 [{state:'running',last_event_age_seconds:901},'stuck'],
 [{state:'running',last_event_age_seconds:900},'running'],
 [{state:'ended',exit:1},'failed'], [{state:'ended_no_report'},'failed'],
 [{state:'interrupted'},'failed'], [{state:'failed'},'failed'],
 [{state:'ended',display_state:'Ended · resumed 1/2'},'finished'],
 [{state:'ended_no_report',display_state:'Ended · resumed 1/2',badge:'amber'},'failed'],
 [{state:'running',display_state:'Running · resumed 1/2'},'running'],
 [{state:'ended',display_state:'Failed · resumed 1/2'},'failed']
];
for (const [s,want] of cases) {
 const value = ctx.laneIndicator(s);
 if (value.kind !== want || !value.label) throw new Error('lane indicator mapping');
}
console.log('lane indicator mapping passed');
