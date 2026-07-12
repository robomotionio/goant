function check(a,b){if(a!==b)throw a+" !== "+b;} var s=JSON.stringify({a:1},null,2);check(s.indexOf('\n')>=0,true);check(JSON.stringify([1],null,'  ').indexOf('\n')>=0,true);console.log('PASS');
