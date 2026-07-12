function check(a,b){if(a!==b)throw a+" !== "+b;} check(/\bword\b/.test('a word here'),true);check(/\bword\b/.test('wordy')===false,true);console.log('PASS');
