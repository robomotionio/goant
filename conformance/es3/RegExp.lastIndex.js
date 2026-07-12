function check(a,b){if(a!==b)throw a+" !== "+b;} var r=/a/g;check(r.lastIndex,0);r.exec('aaa');check(r.lastIndex,1);r.exec('aaa');check(r.lastIndex,2);console.log('PASS');
