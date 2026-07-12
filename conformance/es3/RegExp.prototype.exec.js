function check(a,b){if(a!==b)throw a+" !== "+b;} var m=/(\d)(\d)/.exec('x42y');check(m[0],'42');check(m[1],'4');check(m[2],'2');check(m.index,1);check(/z/.exec('abc'),null);console.log('PASS');
